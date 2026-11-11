package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
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
	"github.com/alessiosavi/go-dontwastetime/internal/pipeline"
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
	// A crash left a classify lease behind and a job 'running': both must be recovered.
	if rows, _ := a.s.ClaimUnclassified(ctx, 10, time.Hour, now0); len(rows) != 2 {
		t.Fatal("setup: no leases")
	}
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
	var leased int
	_ = a.s.DB().QueryRow(`SELECT count(*) FROM message WHERE classify_lease IS NOT NULL`).Scan(&leased)
	if leased != 0 {
		t.Fatalf("boot kept %d stale classify leases", leased)
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

func TestCatchUpDigests_EnqueuesEveryMissedWorkingDay(t *testing.T) {
	rome := config.Rome
	tue := time.Date(2026, 11, 10, 8, 0, 0, 0, rome)
	wed9 := time.Date(2026, 11, 11, 9, 0, 0, 0, rome)
	var got []time.Time
	next := catchUpDigests(tue, wed9, func(at time.Time) error { got = append(got, at); return nil })
	if len(got) != 2 || !got[0].Equal(tue) || !got[1].Equal(tue.AddDate(0, 0, 1)) {
		t.Fatalf("enqueued %v, want Tue and Wed 08:00", got)
	}
	if !next.Equal(time.Date(2026, 11, 12, 8, 0, 0, 0, rome)) {
		t.Fatalf("next = %v, want Thu 08:00", next)
	}
	// A failed enqueue keeps the deadline so the next tick retries it.
	if next := catchUpDigests(tue, wed9, func(time.Time) error { return errors.New("busy") }); !next.Equal(tue) {
		t.Fatalf("failed enqueue advanced the deadline to %v", next)
	}
	// After a long absence only the most recent runs are sent.
	got = nil
	catchUpDigests(tue, tue.AddDate(0, 0, 14), func(at time.Time) error { got = append(got, at); return nil })
	if len(got) != maxCatchUp {
		t.Fatalf("after two weeks enqueued %d digests, want %d", len(got), maxCatchUp)
	}
}

func TestParseArgs_FlagsAfterSubcommand(t *testing.T) {
	fs := flag.NewFlagSet("mailboxes", flag.ContinueOnError)
	write := fs.Bool("write-config", false, "")
	pos, err := parseArgs(fs, []string{"discover", "--write-config"})
	if err != nil || len(pos) != 1 || pos[0] != "discover" || !*write {
		t.Fatalf("pos=%v write=%v err=%v", pos, *write, err)
	}
}

func TestRetryDigest_ReenqueuesDeadLetteredToday(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, config.Rome) // Wednesday, after the 08:00 run
	run := todaysRun(now)
	deadLetter := func(a *app) {
		t.Helper()
		if err := a.s.Enqueue(ctx, pipeline.KindDigest, run.Unix(), now); err != nil {
			t.Fatal(err)
		}
		j, err := a.s.Claim(ctx, []string{pipeline.KindDigest}, now)
		if err != nil || j == nil {
			t.Fatalf("claim: %v %v", j, err)
		}
		for range store.MaxAttempts {
			if err := a.s.Fail(ctx, j.ID, "sendMail 503", now); err != nil {
				t.Fatal(err)
			}
		}
	}
	claim := func(a *app) *store.Job {
		t.Helper()
		j, err := a.s.Claim(ctx, []string{pipeline.KindDigest}, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	a := testApp(t, nil)
	deadLetter(a)
	a.retryDigest(ctx, now, false)
	if j := claim(a); j == nil || j.Ref != run.Unix() || j.Attempts != 0 {
		t.Fatalf("dead-lettered digest not re-enqueued: %+v", j)
	}
	a.retryDigest(ctx, now, false) // the new job is running: no duplicate
	if j := claim(a); j != nil {
		t.Fatalf("duplicate digest job %+v", j)
	}

	sent := testApp(t, nil)
	deadLetter(sent)
	if ok, err := sent.s.ClaimDigestSend(ctx, config.Day(run), "p", []string{"x@example.com"}, "{}", now); err != nil || !ok {
		t.Fatalf("mark sent: %v %v", ok, err)
	}
	sent.retryDigest(ctx, now, false)
	if j := claim(sent); j != nil {
		t.Fatalf("a sent day must not be re-enqueued: %+v", j)
	}
}

func TestRetryDigest_DoneWithoutDelivery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, config.Rome)
	run := todaysRun(now)
	// A job completed without delivering (it no-oped while a sibling held the
	// claim, and the sibling was later rejected and dead-lettered).
	setup := func() *app {
		a := testApp(t, nil)
		if err := a.s.Enqueue(ctx, pipeline.KindDigest, run.Unix(), now); err != nil {
			t.Fatal(err)
		}
		j, err := a.s.Claim(ctx, []string{pipeline.KindDigest}, now)
		if err != nil || j == nil {
			t.Fatalf("claim: %v %v", j, err)
		}
		if err := a.s.Complete(ctx, j.ID, now); err != nil {
			t.Fatal(err)
		}
		return a
	}
	claim := func(a *app) *store.Job {
		t.Helper()
		j, err := a.s.Claim(ctx, []string{pipeline.KindDigest}, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	live := setup()
	live.retryDigest(ctx, now, false)
	if j := claim(live); j == nil || j.Ref != run.Unix() {
		t.Fatalf("live: an unsent day with no pending job must be re-enqueued, got %+v", j)
	}

	dry := setup()
	dry.retryDigest(ctx, now, true)
	if j := claim(dry); j != nil {
		t.Fatalf("dry run: a finished preview must not be re-run: %+v", j)
	}
}
