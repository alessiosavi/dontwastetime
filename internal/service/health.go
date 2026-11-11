package service

import (
	"context"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
)

type TokenUsage struct {
	In  int `json:"in"`
	Out int `json:"out"`
}

type HealthResult struct {
	Meta                 Meta        `json:"meta"`
	Mailboxes            []Freshness `json:"mailboxes"`
	QueuePending         int         `json:"queue_pending"`
	QueueFailed          int         `json:"queue_failed"`
	OldestPendingSeconds int         `json:"oldest_pending_seconds"`
	Unclassified         int         `json:"unclassified"`
	Today                TokenUsage  `json:"today"`
	Month                TokenUsage  `json:"month"`
	DailyMaxTokens       int         `json:"daily_max_tokens"`
	MonthlyMaxCost       float64     `json:"monthly_max_cost"`
	TodayCost            *float64    `json:"today_cost"`
	MonthCost            *float64    `json:"month_cost"`
	CostCurrency         string      `json:"cost_currency,omitempty"`
	CostNote             string      `json:"cost_note"`
	DailyBudgetReached   bool        `json:"daily_budget_reached"`
	MonthlyBudgetReached bool        `json:"monthly_budget_reached"`
	BudgetReached        bool        `json:"budget_reached"`
	LastDigestDate       string      `json:"last_digest_date,omitempty"`
	LastDigestSentAt     *time.Time  `json:"last_digest_sent_at"`
}

// Health reports local observations only; it neither probes Graph nor calls
// the classifier. Costs use the same configured USD prices as budget admission.
func (s *Service) Health(ctx context.Context) (HealthResult, error) {
	at := s.now()
	m, err := s.meta(ctx, at)
	if err != nil {
		return HealthResult{}, err
	}
	r := HealthResult{Meta: m, Mailboxes: m.Freshness, Unclassified: m.Unclassified,
		DailyMaxTokens: s.cfg.LLM.DailyMaxTokens, MonthlyMaxCost: s.cfg.LLM.MonthlyMaxCost,
		CostNote: "prezzi non configurati"}
	if r.QueuePending, r.QueueFailed, r.OldestPendingSeconds, err = s.store.QueueDepth(ctx, at); err != nil {
		return HealthResult{}, storeError(err)
	}
	if r.Today.In, r.Today.Out, err = s.store.UsageToday(ctx, config.Day(at)); err != nil {
		return HealthResult{}, storeError(err)
	}
	if r.Month.In, r.Month.Out, err = s.store.UsageMonth(ctx, config.Month(at)); err != nil {
		return HealthResult{}, storeError(err)
	}
	r.DailyBudgetReached = r.DailyMaxTokens > 0 && r.Today.In+r.Today.Out >= r.DailyMaxTokens
	if s.cfg.LLM.PriceInPerMTok != 0 || s.cfg.LLM.PriceOutPerMTok != 0 {
		today, month := s.cfg.LLM.Cost(r.Today.In, r.Today.Out), s.cfg.LLM.Cost(r.Month.In, r.Month.Out)
		r.TodayCost, r.MonthCost, r.CostCurrency, r.CostNote = &today, &month, "USD", "stima dai prezzi configurati"
		r.MonthlyBudgetReached = r.MonthlyMaxCost > 0 && month >= r.MonthlyMaxCost
	}
	r.BudgetReached = r.DailyBudgetReached || r.MonthlyBudgetReached
	if r.LastDigestDate, r.LastDigestSentAt, err = s.store.LastDigest(ctx); err != nil {
		return HealthResult{}, storeError(err)
	}
	return r, nil
}
