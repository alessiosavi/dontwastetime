package service

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

func TestHealth_CostEstimateOnlyWithPrices(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// Rome is already in November while UTC is still in October.
	f.at = time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC)
	f.cfg.LLM.DailyMaxTokens = 4_000_000
	for day, tokens := range map[string][2]int{"2026-10-31": {9_000_000, 9_000_000}, "2026-11-01": {3_000_000, 1_000_000}, "2026-11-02": {1_000_000, 0}} {
		if err := f.db.UsageAdd(ctx, day, tokens[0], tokens[1]); err != nil {
			t.Fatal(err)
		}
	}
	r, err := f.svc.Health(ctx)
	if err != nil || r.Today.In != 3_000_000 || r.Today.Out != 1_000_000 || r.Month.In != 4_000_000 || r.Month.Out != 1_000_000 ||
		r.TodayCost != nil || r.MonthCost != nil || r.CostNote != "prezzi non configurati" || !r.DailyBudgetReached || !r.BudgetReached {
		t.Fatalf("unpriced health=%+v err=%v", r, err)
	}
	for _, prices := range [][2]float64{{.30, 1.20}, {.30, 0}, {0, 1.20}} {
		f.cfg.LLM.PriceInPerMTok, f.cfg.LLM.PriceOutPerMTok = prices[0], prices[1]
		f.cfg.LLM.MonthlyMaxCost = .5
		r, err = f.svc.Health(ctx)
		if err != nil || r.MonthCost == nil || r.TodayCost == nil || r.CostCurrency != "USD" || !strings.Contains(r.CostNote, "stima") ||
			math.Abs(*r.MonthCost-f.cfg.LLM.Cost(r.Month.In, r.Month.Out)) > 1e-9 ||
			math.Abs(*r.TodayCost-f.cfg.LLM.Cost(r.Today.In, r.Today.Out)) > 1e-9 || !r.MonthlyBudgetReached {
			t.Fatalf("prices=%v health=%+v err=%v", prices, r, err)
		}
	}
	f.cfg.LLM.MonthlyMaxCost = 0
	r, err = f.svc.Health(ctx)
	if err != nil || r.MonthlyBudgetReached || r.MonthCost == nil {
		t.Fatalf("disabled monthly cap=%+v err=%v", r, err)
	}
}

func TestHealth_LocalStateAndFreshness(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	lastSync := f.at.Add(-2 * time.Hour)
	if err := f.db.SetSyncResult(ctx, testBox, false, lastSync); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetSyncResult(ctx, testBox, true, f.at); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetMailboxState(ctx, testBox, store.StateDegraded, "accesso negato", f.at); err != nil {
		t.Fatal(err)
	}
	// Configuration, rather than stale DB enabled flags, defines visibility.
	f.cfg.Mailboxes = append(f.cfg.Mailboxes, config.MailboxConfig{Address: "new@example.test", Enabled: true})
	var err error
	f.svc, err = New(f.db, f.cfg, f.svc.taxonomy, Options{Operator: "founder"}, func() time.Time { return f.at })
	if err != nil {
		t.Fatal(err)
	}
	id := f.mail(t, testBox, "pending", "pending", "", f.at.Add(-time.Hour))
	f.mail(t, "disabled@example.test", "hidden", "hidden", "", f.at.Add(-time.Hour))
	if err := f.db.Enqueue(ctx, store.KindClassify, id, f.at.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Enqueue(ctx, store.KindThreadRefresh, id, f.at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB().ExecContext(ctx, `UPDATE queue SET status='failed' WHERE kind=?`, store.KindThreadRefresh); err != nil {
		t.Fatal(err)
	}
	sent := f.at.Add(-24 * time.Hour)
	if err := f.db.RecordDigest(ctx, "2026-11-10", "private-path", nil, "{}", &sent); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.Health(ctx)
	if err != nil || r.Unclassified != 1 || r.QueuePending != 1 || r.QueueFailed != 1 || r.OldestPendingSeconds != 7200 ||
		r.LastDigestDate != "2026-11-10" || r.LastDigestSentAt == nil || !r.LastDigestSentAt.Equal(sent) {
		t.Fatalf("health=%+v err=%v", r, err)
	}
	if len(r.Mailboxes) != 2 || r.Mailboxes[0].State != store.StateDegraded || r.Mailboxes[0].Reason != "accesso negato" ||
		!r.Mailboxes[0].CatchingUp || r.Mailboxes[0].LastSyncCompletedAt == nil || !r.Mailboxes[0].LastSyncCompletedAt.Equal(lastSync) ||
		r.Mailboxes[1].State != store.StateUnknown {
		t.Fatalf("mailboxes=%+v", r.Mailboxes)
	}
	if !r.Meta.AsOf.Equal(f.at) {
		t.Fatalf("as_of=%v", r.Meta.AsOf)
	}
	raw, err := json.Marshal(r)
	if err != nil || strings.Contains(string(raw), "private-path") || strings.Contains(string(raw), "disabled@example.test") {
		t.Fatalf("health JSON=%s err=%v", raw, err)
	}
	if err := f.db.RecordDigest(ctx, "2026-11-11", "private-path", nil, "{}", nil); err != nil {
		t.Fatal(err)
	}
	r, err = f.svc.Health(ctx)
	if err != nil || r.LastDigestDate != "2026-11-11" || r.LastDigestSentAt != nil {
		t.Fatalf("latest dry-run health=%+v err=%v", r, err)
	}
}
