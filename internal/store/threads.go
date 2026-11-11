package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Triage errors distinguish missing records from an obsolete inbound selection.
var (
	ErrNotFound = errors.New("store: not found")
	ErrStale    = errors.New("store: stale inbound")
)

// querier is the shared database surface for standalone and transactional calls.
type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

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
	AckKind              string
	AckInboundAt         *time.Time
	AckInboundIMID       string
	SnoozedUntil         *time.Time
}

// ThreadMessage is one message of a thread with its classification
// (Label empty while unclassified).
type ThreadMessage struct {
	ID                     int64
	InternetMessageID      string
	Mailbox, Direction     string
	From, Subject, Preview string
	To, CC                 []string
	ReceivedAt, InsertedAt time.Time
	IsAuto, IsBulk, IsList bool
	Label                  string
	Urgent, Negative       bool
	ReplyExpected          *bool
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
	return threadMessages(ctx, s.db, key)
}

// ThreadMessagesTx is ThreadMessages inside the caller's transaction.
func ThreadMessagesTx(ctx context.Context, tx *sql.Tx, key string) ([]ThreadMessage, error) {
	return threadMessages(ctx, tx, key)
}

func threadMessages(ctx context.Context, q querier, key string) ([]ThreadMessage, error) {
	rows, err := q.QueryContext(ctx, `SELECT m.id, m.internet_message_id, m.mailbox, m.direction, m.from_addr, m.subject,
		coalesce(m.body_preview, ''), m.to_addrs, m.cc_addrs, m.received_at, m.inserted_at, m.is_auto, m.is_bulk, m.is_list,
		coalesce(c.label, ''), coalesce(c.urgent, 0), coalesce(c.negative, 0), c.reply_expected
		FROM message m LEFT JOIN classification c ON c.message_id = m.id
		WHERE m.thread_key = ? ORDER BY m.received_at, m.id`, key)
	if err != nil {
		return nil, fmt.Errorf("store: thread messages: %w", err)
	}
	defer rows.Close()
	var out []ThreadMessage
	for rows.Next() {
		var m ThreadMessage
		var at, inserted, to, cc string
		if err := rows.Scan(&m.ID, &m.InternetMessageID, &m.Mailbox, &m.Direction, &m.From, &m.Subject, &m.Preview, &to, &cc, &at, &inserted,
			&m.IsAuto, &m.IsBulk, &m.IsList, &m.Label, &m.Urgent, &m.Negative, &m.ReplyExpected); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(to), &m.To); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(cc), &m.CC); err != nil {
			return nil, err
		}
		if m.ReceivedAt, err = parseTS(at); err != nil {
			return nil, err
		}
		if m.InsertedAt, err = parseTS(inserted); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// HasOutboundTo reports whether an outbound message in any thread, received
// before `before`, addressed recipient in To or Cc. recipient is either an exact
// domain or a full address; both comparisons are case-insensitive.
func (s *Store) HasOutboundTo(ctx context.Context, recipient string, before time.Time) (bool, error) {
	return hasOutboundTo(ctx, s.db, recipient, before)
}

// HasOutboundToTx is HasOutboundTo inside the caller's transaction.
func HasOutboundToTx(ctx context.Context, tx *sql.Tx, recipient string, before time.Time) (bool, error) {
	return hasOutboundTo(ctx, tx, recipient, before)
}

func hasOutboundTo(ctx context.Context, q querier, recipient string, before time.Time) (bool, error) {
	match := `lower(substr(value, instr(value, '@') + 1)) = ?`
	if strings.Contains(recipient, "@") {
		match = `lower(value) = ?`
	}
	var known bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM message m WHERE m.direction = 'out' AND m.received_at < ?
		AND EXISTS (SELECT 1 FROM (
			SELECT value FROM json_each(m.to_addrs)
			UNION ALL SELECT value FROM json_each(m.cc_addrs)
		) WHERE `+match+`))`, ts(before), strings.ToLower(recipient)).Scan(&known)
	if err != nil {
		return false, fmt.Errorf("store: outbound recipient history: %w", err)
	}
	return known, nil
}

const threadCols = `thread_key, mailbox, subject, last_inbound_at, last_outbound_at, last_inbound_from,
	coalesce(last_inbound_message_id, 0), needs_reply, reply_due_at, priority, cassetto, muted_at, p0_alerted_day,
	ack_kind, ack_inbound_at, ack_inbound_imid, snoozed_until`

func scanThread(sc rowScanner) (ThreadRow, error) {
	var r ThreadRow
	var in, out, due, muted, ack, snoozed sql.NullString
	if err := sc.Scan(&r.ThreadKey, &r.Mailbox, &r.Subject, &in, &out, &r.LastInboundFrom, &r.LastInboundMessageID,
		&r.NeedsReply, &due, &r.Priority, &r.Cassetto, &muted, &r.P0AlertedDay,
		&r.AckKind, &ack, &r.AckInboundIMID, &snoozed); err != nil {
		return r, err
	}
	var err error
	for _, p := range []struct {
		src sql.NullString
		dst **time.Time
	}{{in, &r.LastInboundAt}, {out, &r.LastOutboundAt}, {due, &r.ReplyDueAt}, {muted, &r.MutedAt}, {ack, &r.AckInboundAt}, {snoozed, &r.SnoozedUntil}} {
		if *p.dst, err = scanTS(p.src); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Thread loads one thread row.
func (s *Store) Thread(ctx context.Context, key string) (ThreadRow, error) {
	return thread(ctx, s.db, key)
}

// ThreadTx is Thread inside the caller's transaction.
func ThreadTx(ctx context.Context, tx *sql.Tx, key string) (ThreadRow, error) {
	return thread(ctx, tx, key)
}

func thread(ctx context.Context, q querier, key string) (ThreadRow, error) {
	r, err := scanThread(q.QueryRowContext(ctx, `SELECT `+threadCols+` FROM thread WHERE thread_key = ?`, key))
	if err != nil {
		return r, fmt.Errorf("store: thread %q: %w", key, err)
	}
	return r, nil
}

// SaveThreadState writes the computed columns; mute, ack and alert state are
// owned by their dedicated mutations and left alone.
func (s *Store) SaveThreadState(ctx context.Context, r ThreadRow, now time.Time) error {
	return saveThreadState(ctx, s.db, r, now)
}

// SaveThreadStateTx is SaveThreadState inside the caller's transaction.
func SaveThreadStateTx(ctx context.Context, tx *sql.Tx, r ThreadRow, now time.Time) error {
	return saveThreadState(ctx, tx, r, now)
}

func saveThreadState(ctx context.Context, q querier, r ThreadRow, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO thread(thread_key, mailbox, subject, last_inbound_at, last_outbound_at,
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

// ackInboundTx validates the operator's logical inbound against the thread's
// current inbound. A mailbox copy may have another numeric id.
func ackInboundTx(ctx context.Context, tx *sql.Tx, key string, inboundID int64) (time.Time, string, error) {
	var selected string
	if err := tx.QueryRowContext(ctx, `SELECT internet_message_id FROM message WHERE id = ?`, inboundID).Scan(&selected); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return time.Time{}, "", fmt.Errorf("store: inbound %d: %w", inboundID, err)
	}
	var current, received sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT m.internet_message_id, m.received_at
		FROM thread t LEFT JOIN message m ON m.id = t.last_inbound_message_id
		WHERE t.thread_key = ?`, key).Scan(&current, &received)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return time.Time{}, "", fmt.Errorf("store: triage thread %q: %w", key, err)
	}
	if !current.Valid || current.String != selected {
		return time.Time{}, "", fmt.Errorf("store: triage thread %q: %w", key, ErrStale)
	}
	at, err := parseTS(received.String)
	return at, current.String, err
}

// MarkHandledTx acknowledges only the current logical inbound and audits the
// operator's note. The caller recomputes the thread in the same transaction.
func MarkHandledTx(ctx context.Context, tx *sql.Tx, key string, inboundID int64, by, note string, now time.Time) error {
	at, imid, err := ackInboundTx(ctx, tx, key, inboundID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE thread SET ack_kind = 'handled', ack_inbound_at = ?,
		ack_inbound_imid = ?, snoozed_until = NULL, ack_by = ?, ack_at = ?, updated_at = ? WHERE thread_key = ?`,
		ts(at), imid, by, ts(now), ts(now), key)
	if err != nil {
		return fmt.Errorf("store: mark handled: %w", err)
	}
	return logActionTx(ctx, tx, by, "mark_handled", "thread:"+key, map[string]any{"inbound_id": inboundID, "note": note}, now)
}

// SnoozeTx acknowledges the current inbound until a future instant, at most
// 90 days ahead. RecomputeTx retains the original obligation's due time.
func SnoozeTx(ctx context.Context, tx *sql.Tx, key string, inboundID int64, until time.Time, by string, now time.Time) error {
	if !until.After(now) || until.After(now.Add(90*24*time.Hour)) {
		return fmt.Errorf("store: snooze until must be in the future and at most 90 days ahead")
	}
	at, imid, err := ackInboundTx(ctx, tx, key, inboundID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE thread SET ack_kind = 'snoozed', ack_inbound_at = ?,
		ack_inbound_imid = ?, snoozed_until = ?, ack_by = ?, ack_at = ?, updated_at = ? WHERE thread_key = ?`,
		ts(at), imid, ts(until), by, ts(now), ts(now), key)
	if err != nil {
		return fmt.Errorf("store: snooze: %w", err)
	}
	return logActionTx(ctx, tx, by, "snooze", "thread:"+key, map[string]any{"inbound_id": inboundID, "until": ts(until)}, now)
}

// ClearAckTx removes the watermark, snooze and acknowledgement attribution.
// Expiry and new mail clear acknowledgements without creating operator actions.
func ClearAckTx(ctx context.Context, tx *sql.Tx, key string) error {
	res, err := tx.ExecContext(ctx, `UPDATE thread SET ack_kind = '', ack_inbound_at = NULL,
		ack_inbound_imid = '', snoozed_until = NULL, ack_by = '', ack_at = NULL WHERE thread_key = ?`, key)
	return threadUpdated(res, err, key)
}

// ReopenTx reverses mute and acknowledgement, auditing the operator's action.
// The caller recomputes the thread in the same transaction.
func ReopenTx(ctx context.Context, tx *sql.Tx, key, by string, now time.Time) error {
	if err := ClearAckTx(ctx, tx, key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE thread SET muted_at = NULL, muted_by = '', updated_at = ? WHERE thread_key = ?`, ts(now), key); err != nil {
		return fmt.Errorf("store: reopen: %w", err)
	}
	return logActionTx(ctx, tx, by, "reopen", "thread:"+key, nil, now)
}

func threadUpdated(res sql.Result, err error, key string) error {
	if err == nil {
		var n int64
		n, err = res.RowsAffected()
		if err == nil && n != 1 {
			err = ErrNotFound
		}
	}
	if err != nil {
		return fmt.Errorf("store: triage thread %q: %w", key, err)
	}
	return nil
}

// ClaimP0Alert atomically marks an eligible thread alerted for day. False means
// it is no longer eligible or was already alerted. prev is restored on failure.
func (s *Store) ClaimP0Alert(ctx context.Context, key, day string) (claimed bool, prev string, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT p0_alerted_day FROM thread WHERE thread_key = ?`, key).Scan(&prev); err != nil {
			return err
		}
		if prev == day {
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE thread SET p0_alerted_day = ? WHERE thread_key = ?
			AND needs_reply = 1 AND priority = 'P0' AND muted_at IS NULL AND ack_kind = ''`, day, key)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		claimed = n == 1 && err == nil
		return err
	})
	return claimed, prev, err
}

// UnclaimP0Alert restores the marker after a failed alert send.
func (s *Store) UnclaimP0Alert(ctx context.Context, key, prev string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE thread SET p0_alerted_day = ? WHERE thread_key = ?`, prev, key)
	return err
}

// ListNeedsReply returns unmuted threads awaiting a reply: pinned (> 7 days)
// first, then P0, P1, P2, each oldest first.
func (s *Store) ListNeedsReply(ctx context.Context) ([]ThreadRow, error) {
	return s.queryThreads(ctx, `WHERE needs_reply = 1 AND muted_at IS NULL
		ORDER BY CASE priority WHEN 'P1+' THEN 0 WHEN 'P0' THEN 1 WHEN 'P1' THEN 2 ELSE 3 END, last_inbound_at, thread_key`)
}

// OpenThreads returns unmuted, unanswered threads in the SLA sweep window,
// plus expired snoozes of any age.
func (s *Store) OpenThreads(ctx context.Context, since, now time.Time) ([]ThreadRow, error) {
	return s.queryThreads(ctx, `WHERE muted_at IS NULL AND (
		(last_inbound_at >= ? AND (last_outbound_at IS NULL OR last_outbound_at < last_inbound_at))
		OR (ack_kind = 'snoozed' AND snoozed_until <= ?)) ORDER BY thread_key`, ts(since), ts(now))
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
