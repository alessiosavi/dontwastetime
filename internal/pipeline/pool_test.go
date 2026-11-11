package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

var now0 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "dwt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// runPool starts p.Run and returns a stop function that waits for it.
func runPool(t *testing.T, p *Pool) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("pool did not stop")
		}
	}
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPool_ProcessesEachJobOnce(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	const jobs = 50
	for i := range jobs {
		_ = s.Enqueue(ctx, "classify", int64(i), now0)
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	p := NewPool(s, 4, map[string]Handler{"classify": func(_ context.Context, j *store.Job) error {
		mu.Lock()
		seen[j.Ref]++
		mu.Unlock()
		return nil
	}}, nil, (&clock{t: now0}).Now)
	p.Idle = 5 * time.Millisecond
	stop := runPool(t, p)
	eventually(t, "all jobs", func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) == jobs })
	stop()
	for ref, n := range seen {
		if n != 1 {
			t.Fatalf("job ref %d handled %d times", ref, n)
		}
	}
}

func TestPool_BudgetRelease_NoAttemptsNoDeadLetter(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	_ = s.Enqueue(ctx, "classify", 1, now0)
	clk := &clock{t: now0}
	var calls atomic.Int32
	p := NewPool(s, 1, map[string]Handler{"classify": func(context.Context, *store.Job) error {
		calls.Add(1)
		return ErrBudgetReached
	}}, nil, clk.Now)
	p.Idle = time.Millisecond
	stop := runPool(t, p)
	for i := int32(1); i <= 10; i++ {
		eventually(t, "budget stop", func() bool { return calls.Load() >= i })
		clk.Advance(16 * time.Minute) // open the next budget window
	}
	stop()
	var attempts int
	var status string
	if err := s.DB().QueryRow(`SELECT attempts, status FROM queue`).Scan(&attempts, &status); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || status != "pending" {
		t.Fatalf("after budget stops: attempts=%d status=%s, want 0 pending", attempts, status)
	}
	if _, failed, _, _ := s.QueueDepth(ctx, clk.Now()); failed != 0 {
		t.Fatalf("budget stop dead-lettered: %d failed", failed)
	}
}

func TestPool_BudgetReleaseWaitsForWindow(t *testing.T) {
	s := openStore(t)
	_ = s.Enqueue(context.Background(), "classify", 1, now0)
	var calls atomic.Int32
	p := NewPool(s, 1, map[string]Handler{"classify": func(context.Context, *store.Job) error {
		calls.Add(1)
		return ErrBudgetReached
	}}, nil, (&clock{t: now0}).Now) // clock frozen: the 15-minute window never opens
	p.Idle = time.Millisecond
	stop := runPool(t, p)
	eventually(t, "first call", func() bool { return calls.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("released job retried inside its window: %d calls", n)
	}
}

func TestPool_WakeChannel(t *testing.T) {
	s := openStore(t)
	wake := make(chan struct{}, 1)
	var calls atomic.Int32
	p := NewPool(s, 1, map[string]Handler{"classify": func(context.Context, *store.Job) error {
		calls.Add(1)
		return nil
	}}, wake, (&clock{t: now0}).Now)
	p.Idle = time.Hour // only the wake signal can trigger a claim
	stop := runPool(t, p)
	defer stop()
	time.Sleep(20 * time.Millisecond) // let the worker go idle
	_ = s.Enqueue(context.Background(), "classify", 1, now0)
	wake <- struct{}{}
	eventually(t, "woken worker", func() bool { return calls.Load() == 1 })
}

func TestPool_HandlerErrorDoesNotKillLoop(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		_ = s.Enqueue(ctx, "classify", int64(i), now0)
	}
	var mu sync.Mutex
	handled := map[int64]bool{}
	p := NewPool(s, 1, map[string]Handler{"classify": func(_ context.Context, j *store.Job) error {
		mu.Lock()
		handled[j.Ref] = true
		mu.Unlock()
		switch j.Ref {
		case 1:
			return errors.New("boom")
		case 2:
			panic("handler bug")
		}
		return nil
	}}, nil, (&clock{t: now0}).Now)
	p.Idle = time.Millisecond
	stop := runPool(t, p)
	eventually(t, "all three handled", func() bool { mu.Lock(); defer mu.Unlock(); return len(handled) == 3 })
	stop()
	var failedAttempts int
	_ = s.DB().QueryRow(`SELECT count(*) FROM queue WHERE attempts = 1 AND status = 'pending'`).Scan(&failedAttempts)
	if failedAttempts != 2 {
		t.Fatalf("error and panic should each consume one attempt: %d rows", failedAttempts)
	}
}

func TestPool_ThrottledReleasesUntilRetryAfter(t *testing.T) {
	s := openStore(t)
	_ = s.Enqueue(context.Background(), "classify", 1, now0)
	var calls atomic.Int32
	p := NewPool(s, 1, map[string]Handler{"classify": func(context.Context, *store.Job) error {
		calls.Add(1)
		return fmt.Errorf("classify: headers: %w", &graph.ErrThrottled{RetryAfter: time.Hour})
	}}, nil, (&clock{t: now0}).Now)
	p.Idle = time.Millisecond
	stop := runPool(t, p)
	eventually(t, "first call", func() bool { return calls.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	stop()
	var attempts int
	var notBefore string
	_ = s.DB().QueryRow(`SELECT attempts, not_before FROM queue`).Scan(&attempts, &notBefore)
	if attempts != 0 || notBefore != now0.Add(time.Hour).Format("2006-01-02T15:04:05.000Z") {
		t.Fatalf("attempts=%d not_before=%s, want 0 and now+1h", attempts, notBefore)
	}
}
