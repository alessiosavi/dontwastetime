package pipeline

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/classify/llmtest"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

type classifyEnv struct {
	s   *store.Store
	f   *graphtest.MailFake
	c   *graph.Client
	llm *llmtest.Server
	h   Handler
	cfg *config.Config
}

func newClassifyEnv(t *testing.T, respond func(llmtest.Request) llmtest.Reply) *classifyEnv {
	t.Helper()
	s, f, c, cfg := setup(t, mbA)
	cfg.LLM = config.LLMConfig{Model: "m", APIKey: "k", DailyMaxTokens: 1_000_000, ConfidenceThreshold: 0.7}
	fake := llmtest.New(t, respond)
	cfg.LLM.URL = fake.URL
	tx, err := classify.LoadTaxonomy("../../taxonomy.json")
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return now0 }
	l := classify.NewLLM(cfg.LLM, s, clock)
	return &classifyEnv{s: s, f: f, c: c, llm: fake, cfg: cfg, h: ClassifyHandler(s, c, l, tx, cfg, clock)}
}

// ingest syncs the fake mailbox into the store (one classify job per message).
func (e *classifyEnv) ingest(t *testing.T, msgs ...graph.Message) {
	t.Helper()
	e.f.Add(msgs...)
	if _, _, err := NewPoller(e.c, e.s, e.cfg.Mailboxes[0], e.cfg, nil).SyncOnce(context.Background(), now0); err != nil {
		t.Fatal(err)
	}
}

// runNext claims the next classify job and runs the handler on it.
func (e *classifyEnv) runNext(t *testing.T) (*store.Job, error) {
	t.Helper()
	j, err := e.s.Claim(context.Background(), []string{KindClassify}, now0)
	if err != nil || j == nil {
		t.Fatalf("no classify job to run: %v", err)
	}
	return j, e.h(context.Background(), j)
}

func (e *classifyEnv) label(t *testing.T, imid string) store.Classification {
	t.Helper()
	var id int64
	if err := e.s.DB().QueryRow(`SELECT id FROM message WHERE internet_message_id = ?`, imid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	c, err := e.s.ClassificationOf(context.Background(), id)
	if err != nil {
		t.Fatalf("message %s not classified: %v", imid, err)
	}
	return c
}

func TestClassifyJob_RulesSkipLLM(t *testing.T) {
	e := newClassifyEnv(t, llmtest.Label("clienti", 0.9))
	m := synth(mbA, 1)[0]
	e.f.Headers[m.ID] = []graph.Header{{Name: "List-Unsubscribe", Value: "<mailto:u@x>"}}
	e.ingest(t, m)
	if _, err := e.runNext(t); err != nil {
		t.Fatal(err)
	}
	if c := e.label(t, m.InternetMessageID); c.Label != "marketing.news" || c.Method != "rule" {
		t.Fatalf("classification: %+v", c)
	}
	if e.llm.CallCount() != 0 {
		t.Fatalf("rule-matched message reached the LLM (%d calls)", e.llm.CallCount())
	}
	if n := count(t, e.s, `SELECT count(*) FROM queue WHERE kind = ?`, KindThreadRefresh); n != 1 {
		t.Fatalf("thread_refresh jobs = %d", n)
	}
}

func TestClassifyJob_FlagsReachStore(t *testing.T) {
	e := newClassifyEnv(t, llmtest.Label("clienti", 0.9))
	m := synth(mbA, 1)[0]
	e.f.Headers[m.ID] = []graph.Header{{Name: "Precedence", Value: "bulk"}, {Name: "List-Id", Value: "<news.example>"}}
	e.ingest(t, m)
	if _, err := e.runNext(t); err != nil {
		t.Fatal(err)
	}
	var auto, bulk, list bool
	_ = e.s.DB().QueryRow(`SELECT is_auto, is_bulk, is_list FROM message`).Scan(&auto, &bulk, &list)
	if auto || !bulk || !list {
		t.Fatalf("flags in store: auto=%v bulk=%v list=%v", auto, bulk, list)
	}
}

func TestClassifyJob_InvoiceByAttachment(t *testing.T) {
	e := newClassifyEnv(t, llmtest.Label("clienti", 0.9))
	m := synth(mbA, 1)[0]
	m.Subject, m.HasAttachments = "Fattura n. 77 Example S.r.l.", true
	e.f.Attachments[m.ID] = []graph.Attachment{{ID: "a1", Name: "fattura77.pdf", ContentType: "application/pdf", Size: 900}}
	e.ingest(t, m)
	if _, err := e.runNext(t); err != nil {
		t.Fatal(err)
	}
	if c := e.label(t, m.InternetMessageID); c.Label != "admin.invoices" || c.Entity != "SRL" || c.Method != "rule" {
		t.Fatalf("classification: %+v", c)
	}
}

func TestClassifyJob_OutboundSkipsHeaders(t *testing.T) {
	e := newClassifyEnv(t, llmtest.Label("clienti", 0.9))
	m := synth(mbA, 1)[0]
	m.Folder = "sentitems"
	e.ingest(t, m)
	if _, err := e.runNext(t); err != nil {
		t.Fatal(err)
	}
	if e.f.HeaderCalls != 0 {
		t.Fatalf("outbound message fetched headers %d times", e.f.HeaderCalls)
	}
	if c := e.label(t, m.InternetMessageID); c.Label != "clienti" || c.Method != "llm" {
		t.Fatalf("outbound classification: %+v", c)
	}
}

func TestClassifyJob_LowConfidenceIsUnsure(t *testing.T) {
	e := newClassifyEnv(t, llmtest.Label("clienti", 0.5))
	m := synth(mbA, 1)[0]
	e.ingest(t, m)
	if _, err := e.runNext(t); err != nil {
		t.Fatal(err)
	}
	if c := e.label(t, m.InternetMessageID); c.Label != classify.Unsure || c.CostTokens == 0 {
		t.Fatalf("classification: %+v", c)
	}
}

func TestClassifyJob_NoDoubleClassificationUnderRace(t *testing.T) {
	var mu sync.Mutex
	seen := map[int64]int{}
	e := newClassifyEnv(t, func(r llmtest.Request) llmtest.Reply {
		mu.Lock()
		for _, it := range r.Items {
			seen[it.ID]++
		}
		mu.Unlock()
		return llmtest.Label("clienti", 0.9)(r)
	})
	const n = 25
	e.ingest(t, synth(mbA, n)...)
	p := NewPool(e.s, 4, map[string]Handler{KindClassify: e.h}, nil, func() time.Time { return now0 })
	p.Idle = time.Millisecond
	stop := runPool(t, p)
	eventually(t, "all classified", func() bool {
		return count(t, e.s, `SELECT count(*) FROM classification`) == n
	})
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatalf("LLM saw %d distinct messages, want %d", len(seen), n)
	}
	for id, k := range seen {
		if k != 1 {
			t.Fatalf("message %d sent to the LLM %d times", id, k)
		}
	}
}

func TestClassifyJob_BudgetStopLeavesPending(t *testing.T) {
	e := newClassifyEnv(t, llmtest.Label("clienti", 0.9))
	e.ingest(t, synth(mbA, 2)...)
	_ = e.s.UsageAdd(context.Background(), config.Day(now0), e.cfg.LLM.DailyMaxTokens, 0)
	j, err := e.runNext(t)
	if !errors.Is(err, ErrBudgetReached) {
		t.Fatalf("want ErrBudgetReached, got %v", err)
	}
	if e.llm.CallCount() != 0 {
		t.Fatal("budget-stopped job called the LLM")
	}
	if n := count(t, e.s, `SELECT count(*) FROM message WHERE classified_at IS NULL AND classify_lease IS NULL`); n != 2 {
		t.Fatalf("messages pending with lease released = %d, want 2", n)
	}
	// What the pool does with the error: release, no attempt.
	if err := e.s.Release(context.Background(), j.ID, now0.Add(BudgetWindow), now0); err != nil {
		t.Fatal(err)
	}
	if n := count(t, e.s, `SELECT attempts FROM queue WHERE id = ?`, j.ID); n != 0 {
		t.Fatalf("attempts = %d", n)
	}
}

func TestClassifyJob_LLM500LeavesPending(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	e := newClassifyEnv(t, func(r llmtest.Request) llmtest.Reply {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			return llmtest.Reply{Status: http.StatusInternalServerError}
		}
		return llmtest.Label("clienti", 0.9)(r)
	})
	e.ingest(t, synth(mbA, 12)...)
	if _, err := e.runNext(t); err == nil {
		t.Fatal("LLM 500 must fail the job")
	}
	if n := count(t, e.s, `SELECT count(*) FROM message WHERE classified_at IS NULL AND classify_lease IS NULL`); n != 12 {
		t.Fatalf("after the 500 all 12 must stay pending and claimable, got %d", n)
	}
	// The next jobs in the same run still make progress (jobs whose message a
	// sibling already classified are no-ops).
	for {
		j, err := e.s.Claim(context.Background(), []string{KindClassify}, now0)
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			break
		}
		if err := e.h(context.Background(), j); err != nil {
			t.Fatalf("job %d: %v", j.ID, err)
		}
	}
	if n := count(t, e.s, `SELECT count(*) FROM classification`); n != 12 {
		t.Fatalf("classified after recovery = %d, want 12", n)
	}
	if n := e.llm.CallCount(); n != 3 {
		t.Fatalf("LLM calls = %d, want 3 (one 500, then batches of 10 and 2)", n)
	}
}

func TestClassifyJob_AlreadyClassifiedIsNoop(t *testing.T) {
	e := newClassifyEnv(t, llmtest.Label("clienti", 0.9))
	e.ingest(t, synth(mbA, 1)...)
	if _, err := e.runNext(t); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Enqueue(context.Background(), KindClassify, 1, now0); err != nil {
		t.Fatal(err)
	}
	before := e.llm.CallCount()
	if _, err := e.runNext(t); err != nil || e.llm.CallCount() != before {
		t.Fatalf("re-run on a classified message: err=%v calls=%d", err, e.llm.CallCount()-before)
	}
}

func TestClassifyJob_ForbiddenHeadersDoNotBlockBatch(t *testing.T) {
	e := newClassifyEnv(t, llmtest.Label("clienti", 0.9))
	e.ingest(t, synth(mbA, 3)...)
	e.f.Forbidden[mbA] = true // access scoping landed after ingest
	if _, err := e.runNext(t); err != nil {
		t.Fatalf("a forbidden header fetch must not fail the batch: %v", err)
	}
	if n := count(t, e.s, `SELECT count(*) FROM classification`); n != 3 {
		t.Fatalf("classified %d of 3 (classified without headers)", n)
	}
}

func TestClassifyJob_ThrottledHeadersReleaseUntilRetryAfter(t *testing.T) {
	e := newClassifyEnv(t, llmtest.Label("clienti", 0.9))
	e.ingest(t, synth(mbA, 1)...)
	e.f.ThrottleHeaders = true
	p := NewPool(e.s, 1, map[string]Handler{KindClassify: e.h}, nil, func() time.Time { return now0 })
	p.Idle = time.Millisecond
	stop := runPool(t, p)
	eventually(t, "job released", func() bool {
		return count(t, e.s, `SELECT count(*) FROM queue WHERE kind='classify' AND status='pending' AND not_before > ?`,
			now0.Format("2006-01-02T15:04:05.000Z")) == 1
	})
	stop()
	var attempts int
	var notBefore string
	_ = e.s.DB().QueryRow(`SELECT attempts, not_before FROM queue WHERE kind='classify'`).Scan(&attempts, &notBefore)
	if attempts != 0 || notBefore != now0.Add(30*time.Second).Format("2006-01-02T15:04:05.000Z") {
		t.Fatalf("throttled job: attempts=%d not_before=%s, want 0 and now+Retry-After", attempts, notBefore)
	}
	if n := count(t, e.s, `SELECT count(*) FROM message WHERE classify_lease IS NOT NULL`); n != 0 {
		t.Fatalf("throttled batch kept %d leases", n)
	}
}

func TestClassifyJob_RetryAfterPartialBatchCoversSiblings(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	e := newClassifyEnv(t, func(r llmtest.Request) llmtest.Reply {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			return llmtest.Reply{Status: http.StatusInternalServerError}
		}
		return llmtest.Label("clienti", 0.9)(r)
	})
	ms := synth(mbA, 2)
	e.f.Headers[ms[0].ID] = []graph.Header{{Name: "List-Unsubscribe", Value: "<mailto:u@x>"}} // A: rule; B: LLM
	e.ingest(t, ms...)
	jobA, err := e.runNext(t) // A saved by rule, B's LLM call fails
	if err == nil {
		t.Fatal("LLM failure must fail job A")
	}
	jobB, _ := e.s.Claim(context.Background(), []string{KindClassify}, now0)
	_ = e.s.Complete(context.Background(), jobB.ID, now0)   // B's own job no-op'd while A held B's lease
	if err := e.h(context.Background(), jobA); err != nil { // A's retry
		t.Fatal(err)
	}
	if n := count(t, e.s, `SELECT count(*) FROM classification`); n != 2 {
		t.Fatalf("A's retry left sibling B unclassified (%d/2)", n)
	}
}
