package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

func TestRelabel_RecordsOperatorAndRecomputes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.svc.taxonomy.Labels["marketing.news"] = classify.Label{Key: "marketing.news", Title: "Newsletter"}
	id := f.obligation(t, "client", "P2", f.at.Add(-25*time.Hour), f.at.Add(-time.Hour), true)
	r, err := f.svc.Relabel(ctx, id, "marketing.news")
	if err != nil || r.Old != "clienti" || r.New != "marketing.news" || r.State != "none" {
		t.Fatalf("relabel=%+v err=%v", r, err)
	}
	var actor string
	if err := f.db.DB().QueryRowContext(ctx, `SELECT actor FROM action_log WHERE action = 'correct'`).Scan(&actor); err != nil || actor != "founder" {
		t.Fatalf("actor=%q err=%v", actor, err)
	}
	c, err := f.db.ClassificationOf(ctx, id)
	if err != nil || c.Method != "correction" || c.Label != r.New {
		t.Fatalf("classification=%+v err=%v", c, err)
	}
	jobs, err := f.db.JobCounts(ctx, store.KindThreadRefresh, id)
	if err != nil || jobs["pending"] != 1 {
		t.Fatalf("refresh jobs=%v err=%v", jobs, err)
	}
	for _, bucket := range []string{"", unclassified, "unknown"} {
		if _, err := f.svc.Relabel(ctx, id, bucket); !errors.Is(err, ErrInvalid) {
			t.Errorf("bucket=%q error=%v, want ErrInvalid", bucket, err)
		}
	}
}

func TestTriage_OutOfScopeThreadNotFound(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.mail(t, "disabled@example.test", "hidden", "hidden", "clienti", f.at.Add(-time.Hour))
	visible := f.mail(t, testBox, "mixed", "visible", "personal", f.at.Add(-time.Hour))
	hiddenCopy := f.mail(t, "disabled@example.test", "mixed", "hidden", "clienti", f.at.Add(-time.Hour))
	for _, ref := range []string{"hidden", fmt.Sprintf("#%d", id), fmt.Sprintf("#%d", hiddenCopy), "missing"} {
		t.Run(ref, func(t *testing.T) {
			calls := []func() error{
				func() error { _, err := f.svc.MarkHandled(ctx, ref, id, ""); return err },
				func() error {
					_, err := f.svc.Snooze(ctx, ref, id, f.at.Add(time.Hour).Format(time.RFC3339))
					return err
				},
				func() error { _, err := f.svc.Mute(ctx, ref); return err },
				func() error { _, err := f.svc.Reopen(ctx, ref); return err },
			}
			for i, call := range calls {
				if err := call(); !errors.Is(err, ErrNotFound) {
					t.Errorf("action %d: %v, want ErrNotFound", i, err)
				}
			}
		})
	}
	for _, id := range []int64{id, hiddenCopy, 99999} {
		if _, err := f.svc.Relabel(ctx, id, "clienti"); !errors.Is(err, ErrNotFound) {
			t.Errorf("relabel %d: %v", id, err)
		}
	}
	if _, err := f.svc.MarkHandled(ctx, "mixed", hiddenCopy, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("hidden inbound in visible thread: %v", err)
	}
	if _, err := f.svc.Snooze(ctx, "mixed", hiddenCopy, f.at.Add(time.Hour).Format(time.RFC3339)); !errors.Is(err, ErrNotFound) {
		t.Errorf("hidden snooze inbound: %v", err)
	}
	var n int
	if err := f.db.DB().QueryRowContext(ctx, `SELECT count(*) FROM action_log`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("out-of-scope writes=%d err=%v", n, err)
	}
	// Personal and unclassified messages authorize thread triage too.
	for _, label := range []string{"personal", ""} {
		id := visible
		if label == "" {
			id = f.mail(t, testBox, "unclassified", "pending", "", f.at.Add(-time.Hour))
		}
		if _, err := f.svc.Mute(ctx, fmt.Sprintf("#%d", id)); err != nil {
			t.Errorf("visible %q: %v", label, err)
		}
	}
}

func TestSnooze_RejectsPastAndBeyond90Days(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.obligation(t, "client", "P2", f.at.Add(-time.Hour), f.at.Add(23*time.Hour), false)
	for _, until := range []string{"", "bad", "2026-02-30", "2026-11-10", f.at.Format(time.RFC3339),
		f.at.Add(-time.Second).Format(time.RFC3339), f.at.Add(90*24*time.Hour + time.Second).Format(time.RFC3339)} {
		if _, err := f.svc.Snooze(ctx, "client", id, until); !errors.Is(err, ErrInvalid) {
			t.Errorf("until=%q error=%v, want ErrInvalid", until, err)
		}
	}
	var n int
	if err := f.db.DB().QueryRowContext(ctx, `SELECT count(*) FROM action_log`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("invalid snooze writes=%d err=%v", n, err)
	}
	r, err := f.svc.Snooze(ctx, "client", id, f.at.Add(90*24*time.Hour).Format(time.RFC3339))
	if err != nil || r.State != "snoozed" {
		t.Fatalf("90-day boundary=%+v err=%v", r, err)
	}
}

func TestMarkHandled_StaleIsErrStale(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	old := f.obligation(t, "client", "P2", f.at.Add(-2*time.Hour), f.at.Add(22*time.Hour), false)
	f.mail(t, testBox, "client", "new inbound", "clienti", f.at.Add(-time.Hour))
	if _, err := threads.Recompute(ctx, f.db, "client", f.cfg, f.at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.MarkHandled(ctx, "client", old, "phone"); !errors.Is(err, ErrStale) {
		t.Fatalf("error=%v, want ErrStale", err)
	}
	if _, err := f.svc.Snooze(ctx, "client", old, "2026-11-12"); !errors.Is(err, ErrStale) {
		t.Fatalf("snooze error=%v, want ErrStale", err)
	}
	var n int
	if err := f.db.DB().QueryRowContext(ctx, `SELECT count(*) FROM action_log`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale writes=%d err=%v", n, err)
	}
}

func TestTriage_ReversibleAndAudited(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.obligation(t, "client", "P2", f.at.Add(-time.Hour), f.at.Add(23*time.Hour), false)
	r, err := f.svc.MarkHandled(ctx, fmt.Sprintf("#%d", id), id, "risolto al telefono")
	if err != nil || r.Thread != "client" || r.State != "handled" || r.DueAt != nil {
		t.Fatalf("handled=%+v err=%v", r, err)
	}
	if _, err := f.svc.Reopen(ctx, "client"); err != nil {
		t.Fatal(err)
	}
	r, err = f.svc.Snooze(ctx, "client", id, "2026-11-12")
	wantUntil := time.Date(2026, 11, 12, 8, 0, 0, 0, config.Rome)
	if err != nil || r.State != "snoozed" || r.SnoozedUntil == nil || !r.SnoozedUntil.Equal(wantUntil) || r.DueAt == nil {
		t.Fatalf("snoozed=%+v err=%v", r, err)
	}
	if r, err = f.svc.Mute(ctx, "client"); err != nil || r.State != "muted" {
		t.Fatalf("muted=%+v err=%v", r, err)
	}
	if r, err = f.svc.Reopen(ctx, "client"); err != nil || r.State != "due" || r.SnoozedUntil != nil || r.DueAt == nil {
		t.Fatalf("reopened=%+v err=%v", r, err)
	}
	var n int
	if err := f.db.DB().QueryRowContext(ctx, `SELECT count(*) FROM action_log WHERE actor = 'founder'`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("audits=%d err=%v", n, err)
	}
	var details string
	if err := f.db.DB().QueryRowContext(ctx, `SELECT details_json FROM action_log WHERE action = 'mark_handled'`).Scan(&details); err != nil || !strings.Contains(details, "risolto al telefono") {
		t.Fatalf("note=%q err=%v", details, err)
	}
}
