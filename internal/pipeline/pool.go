// Package pipeline is dwt's producer/consumer core: per-mailbox pollers
// produce into the durable SQLite queue, and a fixed worker pool claims and
// processes jobs. The wall clock is injected (now func) — never read here.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

// ErrBudgetReached is returned by a handler when the daily LLM budget stops
// it. The pool releases the job (no attempt counted) until the next window.
var ErrBudgetReached = store.ErrBudgetReached

// BudgetWindow is how long a budget-stopped job waits before the next try.
const BudgetWindow = 15 * time.Minute

// Handler processes one claimed job.
type Handler func(ctx context.Context, job *store.Job) error

// Pool is a fixed set of workers looping claim → handle → mark.
type Pool struct {
	s        *store.Store
	workers  int
	handlers map[string]Handler
	kinds    []string
	wake     <-chan struct{}
	now      func() time.Time
	// Idle is the fallback sleep when no job is due; the wake channel is an
	// optimization, not correctness.
	Idle time.Duration
	log  *slog.Logger
}

// NewPool builds a pool. wake may be nil.
func NewPool(s *store.Store, workers int, handlers map[string]Handler, wake <-chan struct{}, now func() time.Time) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{
		s: s, workers: workers, handlers: handlers, wake: wake, now: now,
		kinds: slices.Sorted(maps.Keys(handlers)),
		Idle:  2 * time.Second,
		log:   slog.Default().With("component", "pool"),
	}
}

// Run blocks until ctx is cancelled; it returns nil on a clean shutdown.
func (p *Pool) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := range p.workers {
		wg.Go(func() { p.worker(ctx, i) })
	}
	wg.Wait()
	return nil
}

func (p *Pool) worker(ctx context.Context, n int) {
	idle := time.NewTimer(0)
	defer idle.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := p.s.Claim(ctx, p.kinds, p.now())
		if err != nil && ctx.Err() == nil {
			p.log.Error("claim failed", "worker", n, "err", err)
		}
		if job != nil {
			p.process(ctx, job)
			continue
		}
		idle.Reset(p.Idle)
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-idle.C:
		}
	}
}

func (p *Pool) process(ctx context.Context, job *store.Job) {
	err := p.call(ctx, job)
	// Bookkeeping must land even when shutdown cancels ctx mid-job.
	bctx := context.WithoutCancel(ctx)
	now := p.now()
	var markErr error
	switch {
	case err == nil:
		markErr = p.s.Complete(bctx, job.ID, now)
	case errors.Is(err, ErrBudgetReached):
		p.log.Info("job released: llm budget reached", "job", job.ID, "kind", job.Kind)
		markErr = p.s.Release(bctx, job.ID, now.Add(BudgetWindow), now)
	case ctx.Err() != nil:
		// Shutdown interrupted the job: retry after restart, no attempt spent.
		markErr = p.s.Release(bctx, job.ID, now, now)
	default:
		p.log.Warn("job failed", "job", job.ID, "kind", job.Kind, "attempt", job.Attempts+1, "err", err)
		markErr = p.s.Fail(bctx, job.ID, err.Error(), now)
	}
	if markErr != nil {
		p.log.Error("job bookkeeping failed", "job", job.ID, "err", markErr)
	}
}

// call runs the handler, converting a panic into an error so one bad job
// never kills the worker.
func (p *Pool) call(ctx context.Context, job *store.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	h, ok := p.handlers[job.Kind]
	if !ok {
		return fmt.Errorf("no handler for kind %q", job.Kind)
	}
	return h(ctx, job)
}
