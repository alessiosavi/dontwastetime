package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ThreadRow is one thread's computed reply state.
type ThreadRow struct {
	ThreadKey            string
	Mailbox, Subject     string
	LastInboundAt        *time.Time
	LastOutboundAt       *time.Time
	LastInboundFrom      string
	LastInboundMessageID int64
	NeedsReply           bool
	ReplyDueAt           *time.Time
	Priority, Cassetto   string
	MutedAt              *time.Time
	P0AlertedDay         string
}

// ThreadMessage is one message of a thread with its classification
// (Label empty while unclassified).
type ThreadMessage struct {
	ID                     int64
	Mailbox, Direction     string
	From, Subject, Preview string
	ReceivedAt             time.Time
	IsAuto, IsBulk, IsList bool
	Label                  string
	Urgent, Negative       bool
}

// UpsertThreadFromMessage makes sure the message's thread row exists and
// returns its key. The reply state itself is computed by threads.Recompute.
func (s *Store) UpsertThreadFromMessage(ctx context.Context, messageID int64, now time.Time) (string, error) {
	var key, mailbox, subject string
	err := s.db.QueryRowContext(ctx, `SELECT thread_key, mailbox, subject FROM message WHERE id = ?`, messageID).Scan(&key, &mailbox, &subject)
	if err != nil {
		return "", fmt.Errorf("store: thread of message %d: %w", messageID, err)
	}
	if key == "" {
		return "", fmt.Errorf("store: message %d has no thread key", messageID)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO thread(thread_key, mailbox, subject, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(thread_key) DO NOTHING`, key, mailbox, subject, ts(now))
	if err != nil {
		return "", fmt.Errorf("store: upsert thread: %w", err)
	}
	return key, nil
}

// ThreadMessages returns a thread's messages oldest first.
func (s *Store) ThreadMessages(ctx context.Context, key string) ([]ThreadMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.mailbox, m.direction, m.from_addr, m.subject,
		coalesce(m.body_preview, ''), m.received_at, m.is_auto, m.is_bulk, m.is_list,
		coalesce(c.label, ''), coalesce(c.urgent, 0), coalesce(c.negative, 0)
		FROM message m LEFT JOIN classification c ON c.message_id = m.id
		WHERE m.thread_key = ? ORDER BY m.received_at, m.id`, key)
	if err != nil {
		return nil, fmt.Errorf("store: thread messages: %w", err)
	}
	defer rows.Close()
	var out []ThreadMessage
	for rows.Next() {
		var m ThreadMessage
		var at string
		if err := rows.Scan(&m.ID, &m.Mailbox, &m.Direction, &m.From, &m.Subject, &m.Preview, &at,
			&m.IsAuto, &m.IsBulk, &m.IsList, &m.Label, &m.Urgent, &m.Negative); err != nil {
			return nil, err
		}
		if m.ReceivedAt, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// KnownClientDomain reports whether domain has classified `clienti` inbound
// history in a thread other than exceptThread.
func (s *Store) KnownClientDomain(ctx context.Context, domain, exceptThread string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM message m JOIN classification c ON c.message_id = m.id
		WHERE c.label = 'clienti' AND m.direction = 'in' AND m.thread_key <> ? AND lower(m.from_addr) LIKE ?`,
		exceptThread, "%@"+strings.ToLower(domain)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: known client domain: %w", err)
	}
	return n > 0, nil
}

const threadCols = `thread_key, mailbox, subject, last_inbound_at, last_outbound_at, last_inbound_from,
	coalesce(last_inbound_message_id, 0), needs_reply, reply_due_at, priority, cassetto, muted_at, p0_alerted_day`

func scanThread(sc rowScanner) (ThreadRow, error) {
	var r ThreadRow
	var in, out, due, muted sql.NullString
	if err := sc.Scan(&r.ThreadKey, &r.Mailbox, &r.Subject, &in, &out, &r.LastInboundFrom, &r.LastInboundMessageID,
		&r.NeedsReply, &due, &r.Priority, &r.Cassetto, &muted, &r.P0AlertedDay); err != nil {
		return r, err
	}
	var err error
	for _, p := range []struct {
		src sql.NullString
		dst **time.Time
	}{{in, &r.LastInboundAt}, {out, &r.LastOutboundAt}, {due, &r.ReplyDueAt}, {muted, &r.MutedAt}} {
		if *p.dst, err = scanTS(p.src); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Thread loads one thread row.
func (s *Store) Thread(ctx context.Context, key string) (ThreadRow, error) {
	r, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadCols+` FROM thread WHERE thread_key = ?`, key))
	if err != nil {
		return r, fmt.Errorf("store: thread %q: %w", key, err)
	}
	return r, nil
}

// SaveThreadState writes the computed columns (mute and alert state are
// owned by MuteThread / MarkP0Alerted and left alone).
func (s *Store) SaveThreadState(ctx context.Context, r ThreadRow, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO thread(thread_key, mailbox, subject, last_inbound_at, last_outbound_at,
		last_inbound_from, last_inbound_message_id, needs_reply, reply_due_at, priority, cassetto, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(thread_key) DO UPDATE SET mailbox = excluded.mailbox, subject = excluded.subject,
		last_inbound_at = excluded.last_inbound_at, last_outbound_at = excluded.last_outbound_at,
		last_inbound_from = excluded.last_inbound_from, last_inbound_message_id = excluded.last_inbound_message_id,
		needs_reply = excluded.needs_reply, reply_due_at = excluded.reply_due_at, priority = excluded.priority,
		cassetto = excluded.cassetto, updated_at = excluded.updated_at`,
		r.ThreadKey, r.Mailbox, r.Subject, nullTS(r.LastInboundAt), nullTS(r.LastOutboundAt), r.LastInboundFrom,
		nullID(r.LastInboundMessageID), r.NeedsReply, nullTS(r.ReplyDueAt), r.Priority, r.Cassetto, ts(now))
	if err != nil {
		return fmt.Errorf("store: save thread %q: %w", r.ThreadKey, err)
	}
	return nil
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// MarkP0Alerted records that the thread's P0 alert went out on day.
func (s *Store) MarkP0Alerted(ctx context.Context, key, day string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE thread SET p0_alerted_day = ? WHERE thread_key = ?`, day, key)
	return err
}

// ListNeedsReply returns unmuted threads awaiting a reply: pinned (> 7 days)
// first, then P0, P1, P2, each oldest first.
func (s *Store) ListNeedsReply(ctx context.Context) ([]ThreadRow, error) {
	return s.queryThreads(ctx, `WHERE needs_reply = 1 AND muted_at IS NULL
		ORDER BY CASE priority WHEN 'P1+' THEN 0 WHEN 'P0' THEN 1 WHEN 'P1' THEN 2 ELSE 3 END, last_inbound_at, thread_key`)
}

// OpenThreads returns unmuted threads whose last inbound is unanswered and
// newer than since (the SLA sweep's working set).
func (s *Store) OpenThreads(ctx context.Context, since time.Time) ([]ThreadRow, error) {
	return s.queryThreads(ctx, `WHERE muted_at IS NULL AND last_inbound_at >= ?
		AND (last_outbound_at IS NULL OR last_outbound_at < last_inbound_at) ORDER BY thread_key`, ts(since))
}

func (s *Store) queryThreads(ctx context.Context, where string, args ...any) ([]ThreadRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+threadCols+` FROM thread `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query threads: %w", err)
	}
	defer rows.Close()
	var out []ThreadRow
	for rows.Next() {
		r, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
