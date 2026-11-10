package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// RecordDigest stores (or replaces) the day's digest row. sentAt nil means a
// dry run: rendered and saved, not sent.
func (s *Store) RecordDigest(ctx context.Context, day, path string, recipients []string, healthJSON string, sentAt *time.Time) error {
	rcpt, err := json.Marshal(nonNil(recipients))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO digest(date, path, sent_at, recipients, health_json) VALUES (?,?,?,?,?)
		ON CONFLICT(date) DO UPDATE SET path = excluded.path, sent_at = excluded.sent_at,
		recipients = excluded.recipients, health_json = excluded.health_json`,
		day, path, nullTS(sentAt), string(rcpt), healthJSON)
	if err != nil {
		return fmt.Errorf("store: record digest: %w", err)
	}
	return nil
}
