package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

var now0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func testApp(t *testing.T, api http.HandlerFunc, boxes ...string) *app {
	t.Helper()
	cfg := &config.Config{
		DigestRecipients: []string{"founder@example.com"}, AlertRecipients: []string{"founder@example.com"},
		DigestSender: "admin@example.com", StateDir: t.TempDir(), PollSeconds: 180, PollOverlapMinutes: 30,
		InitialDays: 90, Workers: 1, RetentionDays: 365,
		LLM: config.LLMConfig{URL: "http://llm.invalid", Model: "m", APIKey: "k", DailyMaxTokens: 1000, ConfidenceThreshold: 0.7},
	}
	for _, b := range boxes {
		cfg.Mailboxes = append(cfg.Mailboxes, config.MailboxConfig{Address: b, Enabled: true, Digest: true, PollSeconds: 180})
	}
	s, err := store.Open(filepath.Join(cfg.StateDir, "dwt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	tx, err := classify.LoadTaxonomy("../../taxonomy.json")
	if err != nil {
		t.Fatal(err)
	}
	var c *graph.Client
	if api != nil {
		_, c = graphtest.New(t, api)
	}
	a := &app{cfg: cfg, s: s, c: c, tx: tx, now: func() time.Time { return now0 }}
	a.llm = classify.NewLLM(cfg.LLM, s, a.now)
	return a
}

// okExcept answers 403 for the listed mailboxes and an empty page otherwise.
func okExcept(forbidden ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, f := range forbidden {
			if graphtest.Mailbox(r) == f {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		_, _ = w.Write([]byte(`{"value":[]}`))
	}
}

func TestNextDigestRun_WeekendSkip(t *testing.T) {
	rome := config.Rome
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 11, 13, 9, 0, 0, 0, rome), time.Date(2026, 11, 16, 8, 0, 0, 0, rome)},  // Fri 09:00 → Mon 08:00
		{time.Date(2026, 11, 10, 7, 0, 0, 0, rome), time.Date(2026, 11, 10, 8, 0, 0, 0, rome)},  // Tue 07:00 → Tue 08:00
		{time.Date(2026, 11, 10, 8, 0, 0, 0, rome), time.Date(2026, 11, 11, 8, 0, 0, 0, rome)},  // at 08:00 → next day
		{time.Date(2026, 11, 14, 12, 0, 0, 0, rome), time.Date(2026, 11, 16, 8, 0, 0, 0, rome)}, // Sat → Mon
	}
	for _, c := range cases {
		if got := nextDigestRun(c.now); !got.Equal(c.want) {
			t.Errorf("nextDigestRun(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestServe_BootSweepEnqueuesUnclassified(t *testing.T) {
	a := testApp(t, okExcept(), "a@example.com")
	ctx := context.Background()
	for i, imid := range []string{"<1@x>", "<2@x>"} {
		m := graph.Message{ID: "g" + imid, InternetMessageID: imid, Mailbox: "a@example.com", Folder: "inbox", ReceivedAt: now0.Add(-time.Duration(i) * time.Hour)}
		if _, _, err := a.s.UpsertMessage(ctx, m, "c:"+imid, now0); err != nil {
			t.Fatal(err)
		}
	}
	// A job left 'running' by a crash must come back too.
	_ = a.s.Enqueue(ctx, "thread_refresh", 1, now0)
	_, _ = a.s.Claim(ctx, nil, now0)
	if err := a.boot(ctx); err != nil {
		t.Fatal(err)
	}
	var classify, running int
	_ = a.s.DB().QueryRow(`SELECT count(*) FROM queue WHERE kind='classify' AND status='pending'`).Scan(&classify)
	_ = a.s.DB().QueryRow(`SELECT count(*) FROM queue WHERE status='running'`).Scan(&running)
	if classify != 2 || running != 0 {
		t.Fatalf("after boot: %d classify jobs (want 2), %d running (want 0)", classify, running)
	}
}

func TestServe_AllMailboxesFailExitsNonZero(t *testing.T) {
	a := testApp(t, okExcept("a@example.com", "b@example.com"), "a@example.com", "b@example.com")
	if err := a.boot(context.Background()); err == nil || !strings.Contains(err.Error(), "no mailbox") {
		t.Fatalf("all mailboxes forbidden must refuse to start, got %v", err)
	}
	b := testApp(t, okExcept("a@example.com"), "a@example.com", "b@example.com")
	if err := b.boot(context.Background()); err != nil {
		t.Fatalf("one readable mailbox is enough to start: %v", err)
	}
}

func TestDoctor_RedOnBadMailbox(t *testing.T) {
	a := testApp(t, okExcept("legal@example.com"), "tech@example.com", "legal@example.com")
	var out bytes.Buffer
	if ok := a.doctor(context.Background(), &out); ok {
		t.Fatalf("doctor green with a forbidden mailbox:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "[RED]") || !strings.Contains(out.String(), "legal@example.com") ||
		!strings.Contains(out.String(), "[OK]   mailbox tech@example.com") {
		t.Fatalf("doctor output:\n%s", out.String())
	}
}

func TestDoctor_ReportsOldestPendingAndBudget(t *testing.T) {
	a := testApp(t, okExcept(), "tech@example.com")
	ctx := context.Background()
	_ = a.s.Enqueue(ctx, "classify", 1, now0.Add(-45*time.Minute))
	_ = a.s.UsageAdd(ctx, config.Day(now0), 1000, 0)
	var out bytes.Buffer
	ok := a.doctor(ctx, &out)
	for _, want := range []string{"oldest pending 45m", "budget reached", "explore", "15%"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out.String())
		}
	}
	if !ok {
		t.Errorf("budget reached is a warning, not red:\n%s", out.String())
	}
}

func TestWriteConfigMerge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	orig := `{"mailboxes":[{"address":"admin@example.com","enabled":false,"digest":true,"poll_seconds":300}],
"digest_recipients":["founder@example.com"],"digest_sender":"admin@example.com","top_clients":["x.example"]}`
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := mergeMailboxes(p, []string{"admin@example.com", "new.person@example.com"})
	if err != nil || len(added) != 1 || added[0] != "new.person@example.com" {
		t.Fatalf("added=%v err=%v", added, err)
	}
	var got struct {
		Mailboxes []config.MailboxConfig `json:"mailboxes"`
		Top       []string               `json:"top_clients"`
	}
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Mailboxes) != 2 || got.Mailboxes[0].Enabled || got.Mailboxes[0].PollSeconds != 300 || !got.Mailboxes[0].Digest {
		t.Fatalf("existing entry changed: %+v", got.Mailboxes)
	}
	if n := got.Mailboxes[1]; n.Address != "new.person@example.com" || !n.Enabled || n.Digest {
		t.Fatalf("new entry must be enabled with digest:false, got %+v", n)
	}
	if len(got.Top) != 1 {
		t.Fatalf("other fields lost: %s", raw)
	}
	if _, err := os.Stat(p + ".bak"); err != nil {
		t.Fatalf("no backup written: %v", err)
	}
}

func TestMuteThread_ByMessageID(t *testing.T) {
	a := testApp(t, nil, "a@example.com")
	ctx := context.Background()
	id, _, _ := a.s.UpsertMessage(ctx, graph.Message{ID: "g", InternetMessageID: "<m@x>", Mailbox: "a@example.com", Folder: "inbox", ReceivedAt: now0}, "c:T", now0)
	if _, err := a.s.UpsertThreadFromMessage(ctx, id, now0); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := a.muteThread(ctx, "#1", &out); err != nil {
		t.Fatal(err)
	}
	if row, _ := a.s.Thread(ctx, "c:T"); row.MutedAt == nil {
		t.Fatal("thread not muted via message id")
	}
}
