package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/digest"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/mailbox"
	"github.com/alessiosavi/go-dontwastetime/internal/pipeline"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

const (
	sweepEvery  = 15 * time.Minute
	verifyEvery = time.Hour
	purgeEvery  = 24 * time.Hour
	tick        = time.Minute
)

// alerter wires P0 alerts to the real sender (dryRun: files only).
func (a *app) alerter(dryRun bool) threads.Alerter {
	return func(ctx context.Context, rows []store.ThreadRow) error {
		return digest.Alert(ctx, a.s, a.c, a.cfg, rows, dryRun, a.now())
	}
}

// handlers are the pool's consumers.
func (a *app) handlers(dryRun bool) map[string]pipeline.Handler {
	return map[string]pipeline.Handler{
		pipeline.KindClassify:      pipeline.ClassifyHandler(a.s, a.c, a.llm, a.tx, a.cfg, a.now),
		pipeline.KindThreadRefresh: threads.RefreshHandler(a.s, a.cfg, a.alerter(dryRun), a.now),
		pipeline.KindDigest: func(ctx context.Context, j *store.Job) error {
			return a.runDigest(ctx, time.Unix(j.Ref, 0), dryRun, a.alerter(dryRun))
		},
	}
}

// runDigest refreshes thread states, builds the digest for instant at and
// sends (or, dryRun, saves) it.
func (a *app) runDigest(ctx context.Context, at time.Time, dryRun bool, alert threads.Alerter) error {
	if err := threads.Sweep(ctx, a.s, a.cfg, alert, a.now()); err != nil {
		logger().Warn("thread sweep before digest had errors", "err", err)
	}
	d, err := digest.Build(ctx, a.s, a.cfg, a.tx, at)
	if err != nil {
		return err
	}
	return digest.Send(ctx, a.s, a.c, a.cfg, d, dryRun, a.now())
}

// serve runs everything until ctx ends: boot, pollers, worker pool, and the
// scheduler (digest, SLA sweep + same-day digest retry, hourly verification,
// daily retention).
func (a *app) serve(ctx context.Context, dryRun bool) error {
	if err := a.boot(ctx); err != nil {
		return err
	}
	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	for _, mb := range a.cfg.Mailboxes {
		if !mb.Enabled {
			continue
		}
		p := pipeline.NewPoller(a.c, a.s, mb, a.cfg, wake)
		wg.Go(func() { a.pollLoop(ctx, p, time.Duration(mb.PollSeconds)*time.Second) })
	}
	pool := pipeline.NewPool(a.s, a.cfg.Workers, a.handlers(dryRun), wake, a.now)
	wg.Go(func() { _ = pool.Run(ctx) })
	wg.Go(func() { a.schedule(ctx, dryRun) })
	logger().Info("serving", "mailboxes", len(a.cfg.Mailboxes), "workers", a.cfg.Workers, "dry_run", dryRun)
	wg.Wait()
	return nil
}

// pollLoop syncs one mailbox forever: the configured interval on success,
// Retry-After on 429, exponential backoff (cap 1h) on other errors.
func (a *app) pollLoop(ctx context.Context, p *pipeline.Poller, interval time.Duration) {
	failures := 0
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		wait := interval
		if _, _, err := p.SyncOnce(ctx, a.now()); err != nil && ctx.Err() == nil {
			failures++
			var te *graph.ErrThrottled
			if errors.As(err, &te) {
				wait = te.RetryAfter
			} else {
				wait = mailbox.NextProbeTime(a.now(), failures).Sub(a.now())
			}
			logger().Warn("sync failed", "mailbox", p.Mailbox(), "failures", failures, "retry_in", wait.String(), "err", err)
		} else {
			failures = 0
		}
		timer.Reset(wait)
	}
}

// schedule checks due work once a minute (robust to sleep/wake: due times
// are compared against the wall clock, not counted down).
func (a *app) schedule(ctx context.Context, dryRun bool) {
	now := a.now()
	nextDigest := nextDigestRun(now)
	if todays := todaysRun(now); !todays.IsZero() {
		if sent, err := a.s.DigestSent(ctx, config.Day(todays)); err == nil && !sent {
			_ = a.enqueueDigest(ctx, todays) // missed while down/asleep
		}
	}
	nextSweep, nextVerify, nextPurge := now, now.Add(verifyEvery), now
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		now = a.now()
		nextDigest = catchUpDigests(nextDigest, now, func(at time.Time) error { return a.enqueueDigest(ctx, at) })
		if !now.Before(nextSweep) {
			if err := threads.Sweep(ctx, a.s, a.cfg, a.alerter(dryRun), now); err != nil {
				logger().Warn("thread sweep", "err", err)
			}
			a.retryDigest(ctx, now)
			nextSweep = now.Add(sweepEvery)
		}
		if !now.Before(nextVerify) {
			mailbox.VerifyAll(ctx, a.c, a.s, now)
			nextVerify = now.Add(verifyEvery)
		}
		if !now.Before(nextPurge) {
			if n, err := a.s.PurgeBodies(ctx, now.AddDate(0, 0, -a.cfg.RetentionDays), now); err != nil {
				logger().Warn("retention purge", "err", err)
			} else if n > 0 {
				logger().Info("retention purge", "bodies", n)
			}
			nextPurge = now.Add(purgeEvery)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// retryDigest re-enqueues today's due digest when it is not sent and its
// latest job dead-lettered (or none exists): a daemon that keeps running must
// not lose the day's digest to a burst of send failures. A claimed day
// (including a send that got no answer) and a finished dry run are left alone.
func (a *app) retryDigest(ctx context.Context, now time.Time) {
	run := todaysRun(now)
	if run.IsZero() {
		return
	}
	sent, err := a.s.DigestSent(ctx, config.Day(run))
	if err != nil || sent {
		return
	}
	status, err := a.s.LatestJobStatus(ctx, pipeline.KindDigest, run.Unix())
	if err != nil {
		logger().Warn("digest retry check", "err", err)
		return
	}
	if status != "" && status != "failed" {
		return
	}
	logger().Warn("re-enqueueing today's unsent digest", "at", run, "previous_job", orDash(status))
	_ = a.enqueueDigest(ctx, run)
}

func (a *app) enqueueDigest(ctx context.Context, at time.Time) error {
	err := a.s.Enqueue(ctx, pipeline.KindDigest, at.Unix(), a.now())
	if err != nil {
		logger().Error("enqueue digest; will retry next tick", "at", at, "err", err)
	}
	return err
}

// maxCatchUp bounds how many missed digests go out after a long absence
// (only the most recent ones; older windows are skipped and logged).
const maxCatchUp = 3

// catchUpDigests enqueues every due working-day run from next up to now
// (sleep or downtime can span several), keeping at most the latest
// maxCatchUp. The deadline advances only past runs that were enqueued, so a
// failed enqueue is retried on the next tick. It returns the new deadline.
func catchUpDigests(next, now time.Time, enqueue func(time.Time) error) time.Time {
	var due []time.Time
	for !now.Before(next) {
		due = append(due, next)
		next = nextDigestRun(next)
	}
	if len(due) == 0 {
		return next
	}
	if skip := len(due) - maxCatchUp; skip > 0 {
		logger().Warn("skipping old missed digests", "skipped", skip, "from", due[0], "to", due[skip-1])
		due = due[skip:]
	}
	for i, at := range due {
		if err := enqueue(at); err != nil {
			return due[i]
		}
	}
	return next
}

// todaysRun is today's 08:00 Rome run if today is a working day and it is
// already past; zero otherwise.
func todaysRun(now time.Time) time.Time {
	d := now.In(config.Rome)
	run := time.Date(d.Year(), d.Month(), d.Day(), 8, 0, 0, 0, config.Rome)
	if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday || d.Before(run) {
		return time.Time{}
	}
	return run
}
