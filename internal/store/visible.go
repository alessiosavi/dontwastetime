package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Visibility contains the enabled mailboxes from the current configuration.
type Visibility struct {
	Mailboxes []string
}

// MessageFilter restricts message metadata; Until is exclusive on received_at.
type MessageFilter struct {
	Mailbox, Bucket, From, Domain, Direction string
	HasAttachments                           *bool
	Since, Until                             *time.Time
}

// VisibleMessage includes classification metadata, empty for unclassified mail.
type VisibleMessage struct {
	MessageRow
	Label, Method, Rationale string
	Confidence               float64
	BodyPurged               bool
}

// BucketCount counts messages in one visible mailbox and classification bucket.
type BucketCount struct {
	Mailbox, Bucket string
	N               int
}

// visiblePredicate always restricts the message, never its thread or the stored
// mailbox flags. The caller supplies the current configuration's enabled boxes.
func visiblePredicate(v Visibility, f MessageFilter) (string, []any) {
	if len(v.Mailboxes) == 0 {
		return "0", nil
	}
	clauses := []string{"m.mailbox IN (" + strings.Repeat("?,", len(v.Mailboxes)-1) + "?)"}
	args := make([]any, 0, len(v.Mailboxes))
	for _, box := range v.Mailboxes {
		args = append(args, box)
	}
	for _, filter := range []struct{ column, value string }{
		{"m.mailbox", f.Mailbox}, {"c.label", f.Bucket}, {"m.direction", f.Direction},
	} {
		if filter.value != "" {
			clauses = append(clauses, filter.column+" = ?")
			args = append(args, filter.value)
		}
	}
	if f.From != "" {
		clauses = append(clauses, "lower(m.from_addr) = ?")
		args = append(args, strings.ToLower(f.From))
	}
	if f.Domain != "" {
		clauses = append(clauses, `((m.direction = 'in' AND lower(substr(m.from_addr, instr(m.from_addr, '@'))) = ?)
			OR (m.direction = 'out' AND EXISTS (SELECT 1 FROM (
				SELECT value FROM json_each(m.to_addrs) UNION ALL SELECT value FROM json_each(m.cc_addrs)
			) WHERE lower(substr(value, instr(value, '@'))) = ?)))`)
		domain := "@" + strings.ToLower(f.Domain)
		args = append(args, domain, domain)
	}
	if f.HasAttachments != nil {
		clauses = append(clauses, "m.has_attachments = ?")
		args = append(args, *f.HasAttachments)
	}
	if f.Since != nil {
		clauses = append(clauses, "m.received_at >= ?")
		args = append(args, ts(*f.Since))
	}
	if f.Until != nil {
		clauses = append(clauses, "m.received_at < ?")
		args = append(args, ts(*f.Until))
	}
	return strings.Join(clauses, " AND "), args
}

const visibleMessageCols = `m.id, m.mailbox, m.graph_id, m.folder, m.direction, m.internet_message_id,
	m.conversation_id, m.from_addr, m.from_name, m.to_addrs, m.cc_addrs, m.subject,
	coalesce(m.body_text, ''), coalesce(m.body_preview, ''), m.received_at, m.inserted_at, m.sent_at,
	m.has_attachments, m.is_auto, m.is_bulk, m.is_list, m.thread_key, m.body_purged_at, m.classified_at,
	coalesce(c.label, ''), coalesce(c.method, ''), coalesce(c.rationale, ''), coalesce(c.confidence, 0)`

const visibleMessageFrom = ` FROM message m LEFT JOIN classification c ON c.message_id = m.id WHERE `

// messageWithExtras lets scanMessage decode the existing row shape while
// scanning the joined classification columns in the same read.
type messageWithExtras struct {
	rowScanner
	extra []any
}

func (sc messageWithExtras) Scan(dest ...any) error {
	return sc.rowScanner.Scan(append(dest, sc.extra...)...)
}

func scanVisibleMessage(sc rowScanner) (VisibleMessage, error) {
	var r VisibleMessage
	var err error
	r.MessageRow, err = scanMessage(messageWithExtras{sc, []any{&r.Label, &r.Method, &r.Rationale, &r.Confidence}})
	r.BodyPurged = r.BodyPurgedAt != nil
	return r, err
}

func (s *Store) queryVisibleMessages(ctx context.Context, where string, args ...any) ([]VisibleMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+visibleMessageCols+visibleMessageFrom+where, args...)
	if err != nil {
		return nil, fmt.Errorf("store: visible messages: %w", err)
	}
	defer rows.Close()
	var out []VisibleMessage
	for rows.Next() {
		r, err := scanVisibleMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// VisibleMessages lists scoped messages newest first, breaking ties by id.
func (s *Store) VisibleMessages(ctx context.Context, v Visibility, f MessageFilter, limit, offset int) ([]VisibleMessage, error) {
	where, args := visiblePredicate(v, f)
	return s.queryVisibleMessages(ctx, where+` ORDER BY m.received_at DESC, m.id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
}

// VisibleMessage treats a missing or out-of-scope id identically.
func (s *Store) VisibleMessage(ctx context.Context, v Visibility, id int64) (VisibleMessage, error) {
	where, args := visiblePredicate(v, MessageFilter{})
	r, err := scanVisibleMessage(s.db.QueryRowContext(ctx,
		`SELECT `+visibleMessageCols+visibleMessageFrom+where+` AND m.id = ?`, append(args, id)...))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return VisibleMessage{}, fmt.Errorf("store: visible message %d: %w", id, err)
	}
	return r, nil
}

// VisibleThreadMessages expands only visible copies, oldest first.
func (s *Store) VisibleThreadMessages(ctx context.Context, v Visibility, key string) ([]VisibleMessage, error) {
	where, args := visiblePredicate(v, MessageFilter{})
	rows, err := s.queryVisibleMessages(ctx, where+` AND m.thread_key = ? ORDER BY m.received_at, m.id`, append(args, key)...)
	if err == nil && len(rows) == 0 {
		err = fmt.Errorf("store: visible thread %q: %w", key, ErrNotFound)
	}
	return rows, err
}

// SearchVisible applies scope and metadata filters before capping raw FTS5
// results. match is the literal MATCH expression produced by FTSQuery.
func (s *Store) SearchVisible(ctx context.Context, v Visibility, match string, f MessageFilter, cap int) ([]SearchHit, error) {
	if cap <= 0 {
		return nil, nil
	}
	where, args := visiblePredicate(v, f)
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, bm25(message_fts)
		FROM message_fts JOIN message m ON m.id = message_fts.rowid
		LEFT JOIN classification c ON c.message_id = m.id
		WHERE `+where+` AND message_fts MATCH ? ORDER BY bm25(message_fts), m.id DESC LIMIT ?`, append(args, match, cap)...)
	if err != nil {
		return nil, fmt.Errorf("store: search visible: %w", err)
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var r SearchHit
		if err := rows.Scan(&r.ID, &r.BM25); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// VisibleObligations includes due and overdue obligations and expired snoozes.
// Visibility and mailbox selection follow the last inbound message; thread
// metadata can refer to a different mailbox after a cross-mailbox update.
func (s *Store) VisibleObligations(ctx context.Context, v Visibility, mailbox, bucket, priority string, now time.Time) ([]ThreadRow, error) {
	visible, args := visiblePredicate(v, MessageFilter{Mailbox: mailbox})
	where := `WHERE reply_due_at IS NOT NULL AND muted_at IS NULL
		AND (ack_kind = '' OR (ack_kind = 'snoozed' AND snoozed_until <= ?))
		AND EXISTS (SELECT 1 FROM message m WHERE m.id = thread.last_inbound_message_id AND ` + visible + `)`
	args = append([]any{ts(now)}, args...)
	if bucket != "" {
		where += ` AND cassetto = ?`
		args = append(args, bucket)
	}
	if priority != "" {
		where += ` AND priority = ?`
		args = append(args, priority)
	}
	return s.queryThreads(ctx, where+` ORDER BY CASE priority WHEN 'P1+' THEN 0 WHEN 'P0' THEN 1 WHEN 'P1' THEN 2 ELSE 3 END, last_inbound_at, thread_key`, args...)
}

// VisibleCounts groups all scoped mail received in [since, until), including
// personal mail and the empty bucket for unclassified messages.
func (s *Store) VisibleCounts(ctx context.Context, v Visibility, since, until time.Time) (counts []BucketCount, unsure, unclassified int, err error) {
	where, args := visiblePredicate(v, MessageFilter{Since: &since, Until: &until})
	rows, err := s.db.QueryContext(ctx, `SELECT m.mailbox, coalesce(c.label, ''), count(*), count(c.message_id)`+
		visibleMessageFrom+where+` GROUP BY m.mailbox, coalesce(c.label, '') ORDER BY m.mailbox, coalesce(c.label, '')`, args...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("store: visible counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r BucketCount
		var classified int
		if err := rows.Scan(&r.Mailbox, &r.Bucket, &r.N, &classified); err != nil {
			return nil, 0, 0, err
		}
		counts = append(counts, r)
		if r.Bucket == "unsure" {
			unsure += r.N
		}
		unclassified += r.N - classified
	}
	return counts, unsure, unclassified, rows.Err()
}
