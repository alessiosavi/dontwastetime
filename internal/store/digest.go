package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// RecordDigest stores (or replaces) the day's digest row. sentAt nil means a
// dry run: rendered and saved, not sent. An existing delivery timestamp is
// never cleared (a preview after a live send must not allow a resend).
func (s *Store) RecordDigest(ctx context.Context, day, path string, recipients []string, healthJSON string, sentAt *time.Time) error {
	rcpt, err := json.Marshal(nonNil(recipients))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO digest(date, path, sent_at, recipients, health_json) VALUES (?,?,?,?,?)
		ON CONFLICT(date) DO UPDATE SET path = excluded.path, sent_at = coalesce(digest.sent_at, excluded.sent_at),
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

// lateCap bounds how old a message may be and still be listed when it is
// classified late (e.g. after a budget pause or the first backfill).
const lateCap = 7 * 24 * time.Hour

// DigestMessages returns classified inbound messages that became visible in
// [from, to): visible = max(received, first classified), so mail classified
// after its window closed appears in the next digest instead of none. Mail
// received more than 7 days before from is never listed (backfill). The
// caller filters by the current config's digest flags; newest first.
func (s *Store) DigestMessages(ctx context.Context, from, to time.Time) ([]DigestItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.mailbox, m.from_addr, m.from_name, m.subject, m.received_at,
		c.label, c.rationale, c.confidence
		FROM message m JOIN classification c ON c.message_id = m.id
		WHERE m.direction = 'in' AND m.first_classified_at IS NOT NULL
		AND max(m.received_at, m.first_classified_at) >= ? AND max(m.received_at, m.first_classified_at) < ?
		AND m.received_at >= ?
		ORDER BY m.received_at DESC, m.id DESC`, ts(from), ts(to), ts(from.Add(-lateCap)))
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

// ClaimDigestSend atomically reserves the day's live delivery (sent_at set
// before the network call). false means another sender already holds it.
// On a failed send call UnclaimDigestSend; a crash in between leaves the day
// marked sent (at most once, never twice).
func (s *Store) ClaimDigestSend(ctx context.Context, day, path string, recipients []string, healthJSON string, now time.Time) (bool, error) {
	rcpt, err := json.Marshal(nonNil(recipients))
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO digest(date, path, sent_at, recipients, health_json) VALUES (?,?,?,?,?)
		ON CONFLICT(date) DO UPDATE SET path = excluded.path, sent_at = excluded.sent_at,
		recipients = excluded.recipients, health_json = excluded.health_json WHERE digest.sent_at IS NULL`,
		day, path, ts(now), string(rcpt), healthJSON)
	if err != nil {
		return false, fmt.Errorf("store: claim digest send: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UnclaimDigestSend releases a reservation whose send failed.
func (s *Store) UnclaimDigestSend(ctx context.Context, day string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE digest SET sent_at = NULL WHERE date = ?`, day)
	return err
}
