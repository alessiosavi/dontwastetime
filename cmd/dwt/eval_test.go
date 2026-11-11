package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/classify/llmtest"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

func evalFixture(t *testing.T, respond func(llmtest.Request) llmtest.Reply) (*app, *graphtest.MailFake, *llmtest.Server) {
	t.Helper()
	a := testApp(t, nil, "a@example.com", "b@example.com")
	f, _, c := graphtest.NewMail(t)
	a.c = c
	l := llmtest.New(t, respond)
	a.cfg.LLM.URL = l.URL
	a.llm = classify.NewLLM(a.cfg.LLM, a.s, a.now)
	return a, f, l
}

func evalMessage(t *testing.T, a *app, n int, mailbox string) int64 {
	t.Helper()
	m := graph.Message{ID: fmt.Sprintf("g%d", n), InternetMessageID: fmt.Sprintf("<%d@secret.example>", n),
		Mailbox: mailbox, Folder: "inbox", From: "sender@secret.example", Subject: "PRIVATE SUBJECT",
		BodyText: "PRIVATE BODY", BodyPreview: "PRIVATE PREVIEW", ReceivedAt: now0}
	id, _, err := a.s.UpsertMessage(context.Background(), m, fmt.Sprintf("c:%d", n), now0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func writeGold(t *testing.T, rows string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gold.jsonl")
	if err := os.WriteFile(p, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Snapshot every cell, including leases/timestamps, rather than just row counts.
func evalSnapshot(t *testing.T, s *store.Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"message", "classification", "thread", "queue", "corrections", "action_log", "mailbox"} {
		rows, err := s.DB().Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var data [][]any
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			data = append(data, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		out[table] = string(raw)
	}
	return out
}

func TestEvalRun_ProductionPathMetricsAndNoStateWrites(t *testing.T) {
	a, f, llm := evalFixture(t, func(r llmtest.Request) llmtest.Reply {
		var answers []llmtest.Answer
		for _, item := range r.Items {
			confidence := 0.9
			if item.Mailbox == "b@example.com" {
				confidence = 0.2
			}
			answers = append(answers, llmtest.Answer{ID: item.ID, Label: "clienti", Confidence: confidence, Reason: "PRIVATE REASON"})
		}
		return llmtest.Reply{Content: llmtest.Results(answers...), In: 101, Out: 23}
	})
	ctx := context.Background()
	id := evalMessage(t, a, 1, "a@example.com")
	if _, err := a.s.DB().Exec(`UPDATE message SET subject = 'PRIVATE CORRECTION SUBJECT' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	evalMessage(t, a, 2, "a@example.com")
	evalMessage(t, a, 3, "b@example.com")
	// Same message id in another mailbox must not select this outbound row.
	_, _, err := a.s.UpsertMessage(ctx, graph.Message{ID: "out", InternetMessageID: "<4@secret.example>", Mailbox: "b@example.com", Folder: "sentitems", ReceivedAt: now0}, "c:out", now0)
	if err != nil {
		t.Fatal(err)
	}
	f.Headers["g1"] = []graph.Header{{Name: "List-Unsubscribe", Value: "PRIVATE HEADER"}}
	// Precedence:junk sets suppression without itself matching a rule.
	f.Headers["g2"] = []graph.Header{{Name: "Precedence", Value: "junk"}}
	if err := a.s.AddCorrection(ctx, id, "clienti", "marketing.news", "test", now0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.UpsertThreadFromMessage(ctx, id, now0); err != nil {
		t.Fatal(err)
	}
	if err := a.s.Enqueue(ctx, "classify", id, now0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.ClaimUnclassified(ctx, 10, time.Hour, now0); err != nil {
		t.Fatal(err)
	}
	before := evalSnapshot(t, a.s)
	gold := writeGold(t, `{"n":1,"mailbox":"a@example.com","internet_message_id":"<1@secret.example>","accept":["marketing.news"],"needs_reply":false}
{"n":2,"mailbox":"a@example.com","internet_message_id":"<2@secret.example>","accept":["business.dev","clienti"],"needs_reply":false}
{"n":3,"mailbox":"b@example.com","internet_message_id":"<3@secret.example>","accept":["clienti"],"needs_reply":true}
{"n":4,"mailbox":"b@example.com","internet_message_id":"<4@secret.example>","accept":["clienti"],"needs_reply":true}
`)
	var out bytes.Buffer
	if err := a.evalCmd(ctx, gold, "other/model", "", &out); err != nil {
		t.Fatal(err)
	}
	if after := evalSnapshot(t, a.s); !reflect.DeepEqual(before, after) {
		t.Fatal("eval changed production tables")
	}
	in, output, err := a.s.UsageToday(ctx, config.Day(now0))
	if err != nil || in != 101 || output != 23 {
		t.Fatalf("usage=%d/%d, err=%v", in, output, err)
	}
	if llm.CallCount() != 1 || llm.Calls[0].Raw["model"] != "other/model" || len(llm.Calls[0].Items) != 2 {
		t.Fatalf("LLM requests: %+v", llm.Calls)
	}
	prompt, _ := json.Marshal(llm.Calls[0].Raw["messages"])
	if !bytes.Contains(prompt, []byte("Human corrections (follow these):")) || !bytes.Contains(prompt, []byte("PRIVATE CORRECTION SUBJECT")) {
		t.Fatal("missing production prompt/correction")
	}
	if a.cfg.LLM.Model != "m" {
		t.Fatal("model override mutated config")
	}
	for _, want := range []string{"Accuracy: 2/3 (66.67%)", "Coverage: 2/3 (66.67%)", "Missing: 1", "LLM tokens: 124 (input 101, output 23)", "| a@example.com | 2/2", "| b@example.com | 0/1", "| rule | 1/1", "| llm | 1/2", "| business.dev | 1/1", "clienti → unsure", "Reply precision: 0/0", "Reply recall: 0/1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in report:\n%s", want, out.String())
		}
	}
	dir := filepath.Join(a.cfg.StateDir, "eval", "runs")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("artifacts=%v, err=%v", entries, err)
	}
	for _, path := range []string{filepath.Join(a.cfg.StateDir, "eval"), dir} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode: %v, %v", st, err)
		}
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("file mode: %v, %v", st, err)
		}
		if !strings.HasPrefix(entry.Name(), "20261110-090000-other_model.") {
			t.Errorf("unsafe/unexpected filename %s", entry.Name())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"PRIVATE", "secret.example", "sender@"} {
			if bytes.Contains(raw, []byte(secret)) || strings.Contains(out.String(), secret) {
				t.Errorf("email content leaked: %s", secret)
			}
		}
		if strings.HasSuffix(path, ".jsonl") {
			lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
			if len(lines) != 4 {
				t.Fatalf("rows=%d", len(lines))
			}
			var row struct {
				N                         int
				Predicted, Method, Status string
				ReplyDecision             bool `json:"reply_decision"`
				Correct                   bool
			}
			if err := json.Unmarshal(lines[1], &row); err != nil {
				t.Fatal(err)
			}
			if row.N != 2 || row.Predicted != "clienti" || row.Method != "llm" || row.ReplyDecision || !row.Correct {
				t.Fatalf("row 2=%+v", row)
			}
			if err := json.Unmarshal(lines[3], &row); err != nil || row.Status != "missing" {
				t.Fatalf("missing row: %+v, %v", row, err)
			}
		}
	}
}

func TestEvalRun_BudgetStopScoresCompletedRows(t *testing.T) {
	a, f, llm := evalFixture(t, llmtest.Label("clienti", 0.9))
	a.cfg.LLM.DailyMaxTokens = 120
	a.llm = classify.NewLLM(a.cfg.LLM, a.s, a.now)
	var gold strings.Builder
	for n := 1; n <= 22; n++ {
		evalMessage(t, a, n, "a@example.com")
		label := "clienti"
		if n == 11 {
			label = "marketing.news"
			f.Headers["g11"] = []graph.Header{{Name: "List-Id", Value: "list"}}
		}
		fmt.Fprintf(&gold, "{\"n\":%d,\"mailbox\":\"a@example.com\",\"internet_message_id\":\"<%d@secret.example>\",\"accept\":[%q],\"needs_reply\":%v}\n", n, n, label, n != 11)
	}
	before := evalSnapshot(t, a.s)
	var out bytes.Buffer
	// Gold order must not change production's oldest-first batching.
	lines := strings.Split(strings.TrimSpace(gold.String()), "\n")
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	if err := a.evalCmd(context.Background(), writeGold(t, strings.Join(lines, "\n")), "", filepath.Join(t.TempDir(), "runs"), &out); err != nil {
		t.Fatal(err)
	}
	if llm.CallCount() != 1 {
		t.Fatalf("budget calls=%d", llm.CallCount())
	}
	if llm.Calls[0].Raw["model"] != "m" || len(llm.Calls[0].Items) != 10 {
		t.Fatal("default model or production batch size not preserved")
	}
	if llm.Calls[0].Items[0].ID != 1 || llm.Calls[0].Items[9].ID != 10 {
		t.Fatal("production oldest-first ordering not preserved")
	}
	for _, want := range []string{"Accuracy: 11/11 (100.00%)", "Budget skipped: 11", "daily budget reached", "Reply precision: 10/10 (100.00%)", "Reply recall: 10/10 (100.00%)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, out.String())
		}
	}
	if !reflect.DeepEqual(before, evalSnapshot(t, a.s)) {
		t.Fatal("budget stop wrote production state")
	}
}

func TestCLI_EvalRequiresRunAndGold(t *testing.T) {
	for _, args := range [][]string{{"eval"}, {"eval", "run"}, {"eval", "other", "--gold", "x"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "dwt eval run") {
			t.Errorf("%v: code=%d stderr=%s", args, code, errb.String())
		}
	}
}

type evalTransport func(*http.Request) (*http.Response, error)

func (f evalTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCLI_EvalModelFlagReachesLLMWithoutConfiguredModel(t *testing.T) {
	a, _, llm := evalFixture(t, llmtest.Label("clienti", 0.9))
	evalMessage(t, a, 1, "a@example.com")
	_, graphServer, _ := graphtest.NewMail(t)
	graphURL, _ := url.Parse(graphServer.URL)
	llmURL, _ := url.Parse(llm.URL)
	// Route the real CLI's Graph defaults to the existing fake. Fail closed
	// on every unexpected host so this integration test cannot call production.
	transport := http.DefaultTransport
	http.DefaultTransport = evalTransport(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		switch r.URL.Host {
		case "graph.microsoft.com", "login.microsoftonline.com":
			r.URL.Scheme, r.URL.Host, r.Host = graphURL.Scheme, graphURL.Host, ""
		case llmURL.Host:
		default:
			return nil, fmt.Errorf("unexpected test host")
		}
		return transport.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = transport })
	for _, key := range []string{"LLM_MODEL", "LLM_DAILY_MAX_TOKENS", "LLM_PRICE_IN_PER_MTOK", "LLM_PRICE_OUT_PER_MTOK", "DWT_STATE_DIR"} {
		t.Setenv(key, "")
	}
	for key, value := range map[string]string{"MS_TENANT_ID": "tenant", "MS_CLIENT_ID": "client", "MS_CLIENT_SECRET": "secret", "LLM_URL": llm.URL, "LLM_API_KEY": "k"} {
		t.Setenv(key, value)
	}
	cfg := *a.cfg
	cfg.LLM.Model = ""
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	gold := writeGold(t, `{"n":1,"mailbox":"a@example.com","internet_message_id":"<1@secret.example>","accept":["clienti"],"needs_reply":true}`)
	var out, errb bytes.Buffer
	code := run([]string{"eval", "run", "--gold", gold, "--model", "flag-model", "--config", path, "--taxonomy", "../../taxonomy.json", "--out", filepath.Join(t.TempDir(), "reports")}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	if llm.CallCount() != 1 || llm.Calls[0].Raw["model"] != "flag-model" {
		t.Fatalf("requests=%+v", llm.Calls)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, unchanged) {
		t.Fatal("model flag changed config file")
	}
}
