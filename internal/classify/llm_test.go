package classify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify/llmtest"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

var now0 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func newLLM(t *testing.T, respond func(llmtest.Request) llmtest.Reply) (*LLM, *llmtest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "dwt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := llmtest.New(t, respond)
	cfg := config.LLMConfig{URL: f.URL + "/v1", Model: "m", APIKey: "k", DailyMaxTokens: 10_000, ConfidenceThreshold: 0.7}
	return NewLLM(cfg, s, func() time.Time { return now0 }), f, s
}

var items = []Item{
	{ID: 1, Mailbox: "business@example.com", From: "ceo@client.example", Subject: "Rinnovo urgente", Preview: "serve entro domani"},
	{ID: 2, Mailbox: "business@example.com", From: "x@vendor.example", Subject: "Proposta", Preview: "ciao"},
}

func TestLLM_JSONModeThenFallback(t *testing.T) {
	l, f, _ := newLLM(t, nil)
	f.Respond = func(r llmtest.Request) llmtest.Reply {
		if r.JSONMode {
			return llmtest.Reply{Status: http.StatusBadRequest}
		}
		return llmtest.Label("clienti", 0.9)(r)
	}
	tx := taxonomy(t)
	res, _, err := l.Classify(context.Background(), items, tx, nil)
	if err != nil || len(res) != 2 || res[0].Label != "clienti" {
		t.Fatalf("fallback: %+v %v", res, err)
	}
	if f.CallCount() != 2 || !f.Calls[0].JSONMode || f.Calls[1].JSONMode {
		t.Fatalf("calls = %d (json modes %v)", f.CallCount(), f.Calls)
	}
	// The endpoint's lack of JSON mode is remembered: next call goes straight without it.
	if _, _, err := l.Classify(context.Background(), items, tx, nil); err != nil {
		t.Fatal(err)
	}
	if f.CallCount() != 3 || f.Calls[2].JSONMode {
		t.Fatalf("JSON mode retried after a 400: %d calls", f.CallCount())
	}
}

func TestLLM_TolerantParser(t *testing.T) {
	l, f, _ := newLLM(t, func(r llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Content: "Ecco la classificazione:\n```json\n" +
			llmtest.Results(llmtest.Answer{ID: 1, Label: "clienti", Confidence: 0.8}, llmtest.Answer{ID: 2, Label: "business.dev", Confidence: 0.75}) +
			"\n```\nFatto."}
	})
	res, _, err := l.Classify(context.Background(), items, taxonomy(t), nil)
	if err != nil || res[0].Label != "clienti" || res[1].Label != "business.dev" {
		t.Fatalf("prose-wrapped JSON: %+v %v", res, err)
	}
	f.Respond = func(llmtest.Request) llmtest.Reply { return llmtest.Reply{Content: "non so"} }
	before := f.CallCount()
	if _, _, err := l.Classify(context.Background(), items, taxonomy(t), nil); err == nil {
		t.Fatal("malformed twice must be an error")
	}
	if n := f.CallCount() - before; n != 2 {
		t.Fatalf("malformed answer retried %d times, want exactly one retry", n-1)
	}
}

func TestLLM_UnknownLabelBecomesUnsure(t *testing.T) {
	l, _, _ := newLLM(t, func(r llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Content: llmtest.Results(
			llmtest.Answer{ID: 1, Label: "spam", Confidence: 0.99},
			llmtest.Answer{ID: 2, Label: "clienti", Confidence: 0.5})}
	})
	res, _, err := l.Classify(context.Background(), items, taxonomy(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Label != Unsure || res[1].Label != Unsure {
		t.Fatalf("unknown label / low confidence must be unsure: %+v", res)
	}
}

func TestLLM_MissingAnswerBecomesUnsure(t *testing.T) {
	l, _, _ := newLLM(t, func(r llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Content: llmtest.Results(llmtest.Answer{ID: 1, Label: "clienti", Confidence: 0.9})}
	})
	res, _, err := l.Classify(context.Background(), items, taxonomy(t), nil)
	if err != nil || len(res) != 2 || res[1].ID != 2 || res[1].Label != Unsure {
		t.Fatalf("every item gets a result: %+v %v", res, err)
	}
}

func TestLLM_UrgentNegativeParsed(t *testing.T) {
	l, _, _ := newLLM(t, func(r llmtest.Request) llmtest.Reply {
		return llmtest.Reply{Content: llmtest.Results(
			llmtest.Answer{ID: 1, Label: "clienti", Confidence: 0.9, Urgent: true, Negative: true, Entity: "SRL"},
			llmtest.Answer{ID: 2, Label: "clienti", Confidence: 0.9, Entity: "ACME"}), In: 300, Out: 40}
	})
	res, u, err := l.Classify(context.Background(), items, taxonomy(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Urgent || !res[0].Negative || res[0].Entity != "SRL" || res[0].Method != MethodLLM {
		t.Errorf("flags: %+v", res[0])
	}
	if res[1].Urgent || res[1].Negative || res[1].Entity != "" {
		t.Errorf("absent flags must be false and unknown entity dropped: %+v", res[1])
	}
	if u.In != 300 || u.Out != 40 {
		t.Errorf("usage = %+v", u)
	}
}

func TestLLM_BudgetReachedNoCall(t *testing.T) {
	l, f, s := newLLM(t, llmtest.Label("clienti", 0.9))
	ctx := context.Background()
	if _, _, err := l.Classify(ctx, items, taxonomy(t), nil); err != nil {
		t.Fatal(err)
	}
	in, out, _ := s.UsageToday(ctx, config.Day(now0))
	if in != 100 || out != 20 {
		t.Fatalf("usage not recorded: %d/%d", in, out)
	}
	_ = s.UsageAdd(ctx, config.Day(now0), 10_000, 0)
	before := f.CallCount()
	if _, _, err := l.Classify(ctx, items, taxonomy(t), nil); !errors.Is(err, store.ErrBudgetReached) {
		t.Fatalf("want ErrBudgetReached, got %v", err)
	}
	if _, _, err := l.Complete(ctx, "riassumi", 100); !errors.Is(err, store.ErrBudgetReached) {
		t.Fatalf("Complete must be budget-checked too, got %v", err)
	}
	if f.CallCount() != before {
		t.Fatal("budget-stopped call reached the endpoint")
	}
}

func monthlyBudgetLLM(t *testing.T) (*LLM, *llmtest.Server, *store.Store) {
	t.Helper()
	l, f, s := newLLM(t, llmtest.Label("clienti", 0.9))
	l.cfg.MonthlyMaxCost = 10
	l.cfg.PriceInPerMTok, l.cfg.PriceOutPerMTok = 0.50, 1.50
	// Each day costs $5, while today's usage is still zero.
	for _, day := range []string{"2026-11-01", "2026-11-09"} {
		if err := s.UsageAdd(context.Background(), day, 4_000_000, 2_000_000); err != nil {
			t.Fatal(err)
		}
	}
	return l, f, s
}

func TestBudget_MonthlyCostCapStops(t *testing.T) {
	for _, spend := range []string{"at cap", "above cap"} {
		t.Run(spend, func(t *testing.T) {
			l, f, s := monthlyBudgetLLM(t)
			ctx := context.Background()
			if spend == "above cap" {
				if err := s.UsageAdd(ctx, "2026-11-02", 1_000_000, 0); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := l.Classify(ctx, items, taxonomy(t), nil); !errors.Is(err, store.ErrBudgetReached) {
				t.Errorf("want ErrBudgetReached at monthly cap, got %v", err)
			}
			if _, _, err := l.Complete(ctx, "riassumi", 100); !errors.Is(err, store.ErrBudgetReached) {
				t.Errorf("Complete must respect monthly cap, got %v", err)
			}
			if n := f.CallCount(); n != 0 {
				t.Errorf("monthly-stopped calls reached endpoint: %d requests", n)
			}
			if in, out, err := s.UsageToday(ctx, "2026-11-10"); err != nil || in != 0 || out != 0 {
				t.Errorf("refused calls reserved tokens: %d/%d, err=%v", in, out, err)
			}
		})
	}
}

func TestBudget_MonthlyCapResetsNextMonth(t *testing.T) {
	l, f, s := monthlyBudgetLLM(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 30, 22, 59, 59, 0, time.UTC)
	l.now = func() time.Time { return now }
	if _, _, err := l.Complete(ctx, "test", 10); !errors.Is(err, store.ErrBudgetReached) {
		t.Fatalf("before Rome month boundary: want ErrBudgetReached, got %v", err)
	}
	if f.CallCount() != 0 {
		t.Fatal("stopped call reached endpoint")
	}
	now = now.Add(time.Second) // Dec 1 in Rome, still Nov 30 UTC.
	if _, _, err := l.Classify(ctx, items, taxonomy(t), nil); err != nil {
		t.Fatalf("next Rome month must allow calls: %v", err)
	}
	if f.CallCount() != 1 {
		t.Fatalf("next month requests = %d, want 1", f.CallCount())
	}
	if in, out, err := s.UsageToday(ctx, "2026-12-01"); err != nil || in != 100 || out != 20 {
		t.Fatalf("next month settlement = %d/%d, err=%v", in, out, err)
	}
}

func TestBudget_MonthlyCapDisabled(t *testing.T) {
	l, f, _ := monthlyBudgetLLM(t)
	l.cfg.MonthlyMaxCost = 0
	if _, _, err := l.Classify(context.Background(), items, taxonomy(t), nil); err != nil {
		t.Fatalf("disabled monthly cap must allow calls: %v", err)
	}
	if f.CallCount() != 1 {
		t.Fatalf("requests = %d, want 1", f.CallCount())
	}
}

func TestLLM_PromptCarriesTaxonomyAndExamples(t *testing.T) {
	l, f, _ := newLLM(t, llmtest.Label("clienti", 0.9))
	ex := []store.Correction{{NewLabel: "eventi.external", Subject: "Vorremmo ospitare un evento", From: "eventi@terzi.example"}}
	if _, _, err := l.Classify(context.Background(), items[:1], taxonomy(t), ex); err != nil {
		t.Fatal(err)
	}
	sys := f.Calls[0].Raw["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, want := range []string{"admin.invoices", "unsure", "Vorremmo ospitare un evento", "eventi.external", "JSON",
		"bulk: true means the mail was sent in bulk", "receipts, payment notices, product or system",
		"Payment receipts and failed payments are admin.invoices.", "Prospects and demos are business.dev",
		"document requests from the external accountant"} {
		if !contains(sys, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if len(f.Calls[0].Items) != 1 || f.Calls[0].Items[0].ID != 1 {
		t.Errorf("user payload items = %+v", f.Calls[0].Items)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestLLM_ReplyExpectedToleratesInvalidFields(t *testing.T) {
	for _, field := range []string{"true", "false", "null", "", `"true"`, "1", "{}", "[]"} {
		t.Run(field, func(t *testing.T) {
			extra := ""
			if field != "" {
				extra = `,"reply_expected":` + field
			}
			l, f, _ := newLLM(t, func(llmtest.Request) llmtest.Reply {
				return llmtest.Reply{Content: fmt.Sprintf(`{"results":[{"id":1,"label":"clienti","confidence":0.9%s}]}`, extra), In: 10, Out: 5}
			})
			res, _, err := l.Classify(context.Background(), items[:1], taxonomy(t), nil)
			if err != nil || f.CallCount() != 1 || len(res) != 1 || res[0].Label != "clienti" {
				t.Fatalf("reply field caused classification failure/retry: %+v %v, calls=%d", res, err, f.CallCount())
			}
			got := res[0].ReplyExpected
			if field == "true" || field == "false" {
				if got == nil || *got != (field == "true") {
					t.Fatalf("reply_expected=%v, want %s", got, field)
				}
			} else if got != nil {
				t.Fatalf("malformed/missing reply_expected=%v; want nil", *got)
			}
			sys := f.Calls[0].Raw["messages"].([]any)[0].(map[string]any)["content"].(string)
			for _, want := range []string{`"reply_expected":`, "reply_expected: true only if someone at the company", "false for automated mail", "cc-only mail", "messages that close a conversation"} {
				if !contains(sys, want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
		})
	}
}

func TestLLM_UsageReservedBeforeCallAndSettled(t *testing.T) {
	l, f, s := newLLM(t, nil)
	day := config.Day(now0)
	f.Respond = func(r llmtest.Request) llmtest.Reply {
		raw, _ := json.Marshal(r.Raw)
		in, out, err := s.UsageToday(context.Background(), day)
		var calls int
		callErr := s.DB().QueryRow("SELECT calls FROM llm_usage WHERE day=?", day).Scan(&calls)
		if err != nil || callErr != nil || calls != 1 || in != len(raw)/4 || out != 0 {
			t.Errorf("before response: calls=%d tokens=%d/%d, err=%v/%v, estimate=%d", calls, in, out, err, callErr, len(raw)/4)
		}
		return llmtest.Reply{Content: "done", In: 3, Out: 2}
	}
	if _, u, err := l.Complete(context.Background(), "test", 10); err != nil || u != (Usage{In: 3, Out: 2}) {
		t.Fatalf("completion usage=%+v, err=%v", u, err)
	}
	in, out, err := s.UsageToday(context.Background(), day)
	var calls int
	callErr := s.DB().QueryRow("SELECT calls FROM llm_usage WHERE day=?", day).Scan(&calls)
	if err != nil || callErr != nil || in != 3 || out != 2 || calls != 1 {
		t.Fatalf("settled: calls=%d tokens=%d/%d err=%v/%v", calls, in, out, err, callErr)
	}
}

func TestLLM_FailedCallsKeepReservationAndHitCap(t *testing.T) {
	for _, failure := range []string{"http500", "timeout", "cancel", "reset", "malformed-envelope", "read-error"} {
		t.Run(failure, func(t *testing.T) {
			l, _, s := newLLM(t, llmtest.Label("clienti", 0.9))
			cancelCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			estimates := make(chan int, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				estimates <- len(raw) / 4
				switch failure {
				case "http500":
					w.WriteHeader(http.StatusInternalServerError)
				case "timeout":
					<-r.Context().Done()
				case "cancel":
					// Deterministically cancel after the server receives the request.
					cancel()
					<-r.Context().Done()
				case "reset":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
				case "malformed-envelope":
					_, _ = io.WriteString(w, "{}")
				case "read-error":
					w.Header().Set("Content-Length", "1000")
					_, _ = io.WriteString(w, "{")
				}
			}))
			defer srv.Close()
			l.cfg.URL = srv.URL
			l.cfg.DailyMaxTokens = 1
			if failure == "timeout" {
				l.http.Timeout = 200 * time.Millisecond
			}
			if _, _, err := l.Complete(cancelCtx, "test", 10); err == nil {
				t.Fatal("failed request succeeded")
			} else if failure == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected deadline failure, got %v", err)
			}
			estimate := <-estimates
			in, out, err := s.UsageToday(context.Background(), config.Day(now0))
			if err != nil || in != estimate || out != 0 {
				t.Fatalf("failed-call tokens=%d/%d, want %d/0, err=%v", in, out, estimate, err)
			}
			if _, _, err := l.Complete(context.Background(), "test", 10); !errors.Is(err, store.ErrBudgetReached) {
				t.Fatalf("failed call did not exhaust cap: %v", err)
			}
		})
	}
}

func TestLLM_JSONFallbackReservesBothCalls(t *testing.T) {
	l, f, s := newLLM(t, nil)
	f.Respond = func(r llmtest.Request) llmtest.Reply {
		if r.JSONMode {
			return llmtest.Reply{Status: http.StatusBadRequest}
		}
		return llmtest.Label("clienti", 0.9)(r)
	}
	_, u, err := l.Classify(context.Background(), items, taxonomy(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f.Calls[0].Raw)
	in, out, err := s.UsageToday(context.Background(), config.Day(now0))
	var calls int
	callErr := s.DB().QueryRow("SELECT calls FROM llm_usage WHERE day=?", config.Day(now0)).Scan(&calls)
	if err != nil || callErr != nil || calls != 2 || in != len(raw)/4+100 || out != 20 || u.In != in || u.Out != out {
		t.Fatalf("fallback calls=%d tokens=%d/%d usage=%+v, err=%v/%v", calls, in, out, u, err, callErr)
	}
}

func TestLLM_UsageSettlesOnReservationDay(t *testing.T) {
	l, f, s := newLLM(t, nil)
	var crossedMidnight atomic.Bool
	l.now = func() time.Time {
		if crossedMidnight.Load() {
			return now0.Add(24 * time.Hour)
		}
		return now0
	}
	f.Respond = func(r llmtest.Request) llmtest.Reply {
		crossedMidnight.Store(true)
		return llmtest.Label("clienti", 0.9)(r)
	}
	if _, _, err := l.Classify(context.Background(), items, taxonomy(t), nil); err != nil {
		t.Fatal(err)
	}
	in, out, err := s.UsageToday(context.Background(), config.Day(now0))
	if err != nil || in != 100 || out != 20 {
		t.Fatalf("reservation day=%d/%d, err=%v", in, out, err)
	}
	in, out, err = s.UsageToday(context.Background(), config.Day(now0.Add(24*time.Hour)))
	if err != nil || in != 0 || out != 0 {
		t.Fatalf("response day=%d/%d, err=%v", in, out, err)
	}
}

func TestLLM_ReservationFailurePreventsCall(t *testing.T) {
	l, f, s := newLLM(t, llmtest.Label("clienti", 0.9))
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_usage BEFORE INSERT ON llm_usage BEGIN SELECT RAISE(FAIL, 'test reservation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Complete(context.Background(), "test", 10); err == nil || f.CallCount() != 0 {
		t.Fatalf("reservation failure: err=%v, calls=%d", err, f.CallCount())
	}
}
