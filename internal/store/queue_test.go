package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestClaim_ExclusiveUnderRace(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const jobs = 200
	for i := range jobs {
		if err := s.Enqueue(ctx, "classify", int64(i), now0); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				j, err := s.Claim(ctx, []string{"classify"}, now0)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
				if err := s.Complete(ctx, j.ID, now0); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if len(seen) != jobs {
		t.Fatalf("processed %d distinct jobs, want %d", len(seen), jobs)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %d processed %d times", id, n)
		}
	}
}

func TestClaim_FiltersKinds(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.Enqueue(ctx, "digest", 1, now0)
	if j, _ := s.Claim(ctx, []string{"classify"}, now0); j != nil {
		t.Fatalf("claimed a kind not asked for: %+v", j)
	}
	if j, _ := s.Claim(ctx, []string{"classify", "digest"}, now0); j == nil || j.Kind != "digest" || j.Ref != 1 {
		t.Fatalf("claim = %+v", j)
	}
}

func TestClaim_RespectsNotBefore(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.Enqueue(ctx, "classify", 7, now0)
	j, _ := s.Claim(ctx, nil, now0)
	if err := s.Release(ctx, j.ID, now0.Add(15*time.Minute), now0); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.Claim(ctx, nil, now0.Add(14*time.Minute)); j != nil {
		t.Fatalf("claimed before not_before: %+v", j)
	}
	if j, _ := s.Claim(ctx, nil, now0.Add(15*time.Minute)); j == nil || j.Ref != 7 || j.Attempts != 0 {
		t.Fatalf("claim at not_before = %+v", j)
	}
}

func TestFail_BackoffThenDeadLetter(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.Enqueue(ctx, "classify", 1, now0)
	at := now0
	for i := 1; i <= 5; i++ {
		j, err := s.Claim(ctx, nil, at)
		if err != nil || j == nil {
			t.Fatalf("attempt %d: claim %v %v", i, j, err)
		}
		if j.Attempts != i-1 {
			t.Fatalf("attempt %d: attempts = %d", i, j.Attempts)
		}
		if err := s.Fail(ctx, j.ID, "boom", at); err != nil {
			t.Fatal(err)
		}
		if i < 5 {
			backoff := time.Duration(1<<i) * time.Minute
			if j, _ := s.Claim(ctx, nil, at.Add(backoff-time.Second)); j != nil {
				t.Fatalf("attempt %d: reclaimed before %s backoff", i, backoff)
			}
			at = at.Add(backoff)
		}
	}
	if j, _ := s.Claim(ctx, nil, at.Add(24*time.Hour)); j != nil {
		t.Fatalf("dead-lettered job claimed: %+v", j)
	}
	pending, failed, _, err := s.QueueDepth(ctx, at)
	if err != nil || pending != 0 || failed != 1 {
		t.Fatalf("depth pending=%d failed=%d err=%v", pending, failed, err)
	}
}

func TestQueueDepth_OldestPending(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.Enqueue(ctx, "classify", 1, now0.Add(-90*time.Second))
	_ = s.Enqueue(ctx, "classify", 2, now0)
	pending, failed, oldest, err := s.QueueDepth(ctx, now0)
	if err != nil || pending != 2 || failed != 0 || oldest != 90 {
		t.Fatalf("pending=%d failed=%d oldest=%d err=%v", pending, failed, oldest, err)
	}
}

func TestResetRunning_RecoversCrashedJobs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.Enqueue(ctx, "classify", 1, now0)
	if j, _ := s.Claim(ctx, nil, now0); j == nil {
		t.Fatal("no claim")
	}
	n, err := s.ResetRunning(ctx, now0)
	if err != nil || n != 1 {
		t.Fatalf("reset %d %v", n, err)
	}
	if j, _ := s.Claim(ctx, nil, now0); j == nil || j.Attempts != 0 {
		t.Fatalf("recovered job = %+v", j)
	}
}

func TestEnqueueTx_RollsBackWithCaller(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.EnqueueTx(ctx, tx, "classify", 1, now0); err != nil {
			t.Fatal(err)
		}
		return errors.New("rollback")
	})
	if pending, _, _, _ := s.QueueDepth(ctx, now0); pending != 0 {
		t.Fatalf("enqueue survived rollback: %d pending", pending)
	}
}
