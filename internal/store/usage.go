package store

import (
	"context"
	"fmt"
)

// UsageAdd records one LLM call's tokens against a day ("YYYY-MM-DD").
func (s *Store) UsageAdd(ctx context.Context, day string, in, out int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO llm_usage(day, calls, in_tokens, out_tokens) VALUES (?,1,?,?)
		ON CONFLICT(day) DO UPDATE SET calls = calls + 1, in_tokens = in_tokens + excluded.in_tokens,
		out_tokens = out_tokens + excluded.out_tokens`, day, in, out)
	if err != nil {
		return fmt.Errorf("store: usage add: %w", err)
	}
	return nil
}

// UsageToday returns the day's token totals (zero when no call yet).
func (s *Store) UsageToday(ctx context.Context, day string) (in, out int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT coalesce(sum(in_tokens),0), coalesce(sum(out_tokens),0) FROM llm_usage WHERE day = ?`, day).Scan(&in, &out)
	if err != nil {
		return 0, 0, fmt.Errorf("store: usage today: %w", err)
	}
	return in, out, nil
}

// UsageMonth returns token totals for days in month ("YYYY-MM"), or zero
// when no calls have been recorded in that month.
func (s *Store) UsageMonth(ctx context.Context, month string) (in, out int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT coalesce(sum(in_tokens),0), coalesce(sum(out_tokens),0) FROM llm_usage WHERE substr(day,1,7) = ?`, month).Scan(&in, &out)
	if err != nil {
		return 0, 0, fmt.Errorf("store: usage month: %w", err)
	}
	return in, out, nil
}

// UsageAdjust settles a previously reserved call without incrementing calls.
// Deltas may be negative when the input estimate exceeded actual usage.
func (s *Store) UsageAdjust(ctx context.Context, day string, in, out int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE llm_usage SET in_tokens = in_tokens + ?, out_tokens = out_tokens + ? WHERE day = ?`, in, out, day)
	if err != nil {
		return fmt.Errorf("store: usage adjust: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("store: usage adjust: reservation missing")
	}
	return nil
}
