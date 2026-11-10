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

// DigestItem is one classified inbound message of a digest-enabled mailbox.
type DigestItem struct {
	ID                      int64
	Mailbox, From, FromName string
	Subject                 string
	ReceivedAt              time.Time
	Label, Rationale        string
	Confidence              float64
}

// DigestMessages returns classified inbound messages received in [from, to)
// on enabled mailboxes whose digest flag is on, newest first.
func (s *Store) DigestMessages(ctx context.Context, from, to time.Time) ([]DigestItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.mailbox, m.from_addr, m.from_name, m.subject, m.received_at,
		c.label, c.rationale, c.confidence
		FROM message m JOIN classification c ON c.message_id = m.id JOIN mailbox b ON b.address = m.mailbox
		WHERE m.direction = 'in' AND b.digest = 1 AND b.enabled = 1 AND m.received_at >= ? AND m.received_at < ?
		ORDER BY m.received_at DESC, m.id DESC`, ts(from), ts(to))
	if err != nil {
		return nil, fmt.Errorf("store: digest messages: %w", err)
	}
	defer rows.Close()
	var out []DigestItem
	for rows.Next() {
		var d DigestItem
		var at string
		if err := rows.Scan(&d.ID, &d.Mailbox, &d.From, &d.FromName, &d.Subject, &at, &d.Label, &d.Rationale, &d.Confidence); err != nil {
			return nil, err
		}
		if d.ReceivedAt, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UnclassifiedCount is the number of messages still waiting for a label.
func (s *Store) UnclassifiedCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM message WHERE classified_at IS NULL`).Scan(&n)
	return n, err
}

// DigestSent reports whether the day's digest already went out.
func (s *Store) DigestSent(ctx context.Context, day string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM digest WHERE date = ? AND sent_at IS NOT NULL`, day).Scan(&n)
	return n > 0, err
}
