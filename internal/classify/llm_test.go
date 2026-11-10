package classify

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
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

func TestLLM_PromptCarriesTaxonomyAndExamples(t *testing.T) {
	l, f, _ := newLLM(t, llmtest.Label("clienti", 0.9))
	ex := []store.Correction{{NewLabel: "eventi.external", Subject: "Vorremmo ospitare un evento", From: "eventi@terzi.example"}}
	if _, _, err := l.Classify(context.Background(), items[:1], taxonomy(t), ex); err != nil {
		t.Fatal(err)
	}
	sys := f.Calls[0].Raw["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, want := range []string{"admin.invoices", "unsure", "Vorremmo ospitare un evento", "eventi.external", "JSON"} {
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
