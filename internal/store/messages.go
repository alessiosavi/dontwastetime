package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/graph"
)

// Folders dwt reads; anything else is refused.
const (
	FolderInbox     = "inbox"
	FolderSentItems = "sentitems"
)

// MessageRow is one stored message.
type MessageRow struct {
	ID                     int64
	Mailbox, GraphID       string
	Folder, Direction      string
	InternetMessageID      string
	ConversationID         string
	From, FromName         string
	To, CC                 []string
	Subject                string
	BodyText, BodyPreview  string
	ReceivedAt, InsertedAt time.Time
	SentAt                 *time.Time
	HasAttachments         bool
	IsAuto, IsBulk, IsList bool
	ThreadKey              string
	BodyPurgedAt           *time.Time
	ClassifiedAt           *time.Time
}

// Graph converts the row back to the graph shape the rules engine consumes.
func (r MessageRow) Graph() graph.Message {
	m := graph.Message{
		ID: r.GraphID, InternetMessageID: r.InternetMessageID, ConversationID: r.ConversationID,
		Mailbox: r.Mailbox, Folder: r.Folder, Subject: r.Subject, From: r.From, FromName: r.FromName,
		BodyText: r.BodyText, BodyPreview: r.BodyPreview, To: r.To, CC: r.CC, ReceivedAt: r.ReceivedAt,
		HasAttachments: r.HasAttachments, IsAuto: r.IsAuto, IsBulk: r.IsBulk, IsList: r.IsList,
	}
	if r.SentAt != nil {
		m.SentAt = *r.SentAt
	}
	return m
}

func direction(folder string) (string, error) {
	switch folder {
	case FolderInbox:
		return "in", nil
	case FolderSentItems:
		return "out", nil
	}
	return "", fmt.Errorf("store: unsupported folder %q", folder)
}

// UpsertMessage is UpsertMessageTx in its own transaction.
func (s *Store) UpsertMessage(ctx context.Context, m graph.Message, threadKey string, now time.Time) (id int64, inserted bool, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		id, inserted, err = s.UpsertMessageTx(ctx, tx, m, threadKey, now)
		return err
	})
	return id, inserted, err
}

// UpsertMessageTx inserts m keyed by (mailbox, internet_message_id), or
// refreshes the mutable fields of the existing row (inserted=false). Folder
// and direction are fixed at first sight; a purged body is never restored.
func (s *Store) UpsertMessageTx(ctx context.Context, tx *sql.Tx, m graph.Message, threadKey string, now time.Time) (int64, bool, error) {
	dir, err := direction(m.Folder)
	if err != nil {
		return 0, false, err
	}
	if m.InternetMessageID == "" || m.Mailbox == "" {
		return 0, false, fmt.Errorf("store: message %s lacks mailbox or internetMessageId", m.ID)
	}
	to, _ := json.Marshal(nonNil(m.To))
	cc, _ := json.Marshal(nonNil(m.CC))
	var sent any
	if !m.SentAt.IsZero() {
		sent = ts(m.SentAt)
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM message WHERE mailbox = ? AND internet_message_id = ?`,
		m.Mailbox, m.InternetMessageID).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx, `INSERT INTO message (mailbox, graph_id, folder, internet_message_id, conversation_id,
			direction, from_addr, from_name, to_addrs, cc_addrs, subject, body_text, body_preview, received_at, sent_at,
			has_attachments, thread_key, inserted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			m.Mailbox, m.ID, m.Folder, m.InternetMessageID, m.ConversationID, dir, m.From, m.FromName,
			string(to), string(cc), m.Subject, m.BodyText, m.BodyPreview, ts(m.ReceivedAt), sent,
			m.HasAttachments, threadKey, ts(now))
		if err != nil {
			return 0, false, fmt.Errorf("store: insert message: %w", err)
		}
		id, err = res.LastInsertId()
		return id, true, err
	case err != nil:
		return 0, false, fmt.Errorf("store: lookup message: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE message SET graph_id = ?, conversation_id = ?, from_addr = ?, from_name = ?,
		to_addrs = ?, cc_addrs = ?, subject = ?, has_attachments = ?, thread_key = ?,
		body_text = CASE WHEN body_purged_at IS NULL THEN ? ELSE NULL END,
		body_preview = CASE WHEN body_purged_at IS NULL THEN ? ELSE NULL END
		WHERE id = ?`,
		m.ID, m.ConversationID, m.From, m.FromName, string(to), string(cc), m.Subject, m.HasAttachments,
		threadKey, m.BodyText, m.BodyPreview, id)
	if err != nil {
		return 0, false, fmt.Errorf("store: update message: %w", err)
	}
	return id, false, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

const messageCols = `id, mailbox, graph_id, folder, direction, internet_message_id, conversation_id, from_addr, from_name,
	to_addrs, cc_addrs, subject, coalesce(body_text,''), coalesce(body_preview,''), received_at, inserted_at, sent_at,
	has_attachments, is_auto, is_bulk, is_list, thread_key, body_purged_at, classified_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanMessage(sc rowScanner) (MessageRow, error) {
	var r MessageRow
	var to, cc, received, inserted string
	var sent, purged, classified sql.NullString
	err := sc.Scan(&r.ID, &r.Mailbox, &r.GraphID, &r.Folder, &r.Direction, &r.InternetMessageID, &r.ConversationID,
		&r.From, &r.FromName, &to, &cc, &r.Subject, &r.BodyText, &r.BodyPreview, &received, &inserted, &sent,
		&r.HasAttachments, &r.IsAuto, &r.IsBulk, &r.IsList, &r.ThreadKey, &purged, &classified)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(to), &r.To); err != nil {
		return r, fmt.Errorf("store: message %d to_addrs: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(cc), &r.CC); err != nil {
		return r, fmt.Errorf("store: message %d cc_addrs: %w", r.ID, err)
	}
	if r.ReceivedAt, err = parseTS(received); err != nil {
		return r, err
	}
	if r.InsertedAt, err = parseTS(inserted); err != nil {
		return r, err
	}
	if r.SentAt, err = scanTS(sent); err != nil {
		return r, err
	}
	if r.BodyPurgedAt, err = scanTS(purged); err != nil {
		return r, err
	}
	r.ClassifiedAt, err = scanTS(classified)
	return r, err
}

func queryMessages(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, where string, args ...any) ([]MessageRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+messageCols+` FROM message `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query messages: %w", err)
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		r, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Message loads one message by row id.
func (s *Store) Message(ctx context.Context, id int64) (MessageRow, error) {
	r, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageCols+` FROM message WHERE id = ?`, id))
	if err != nil {
		return r, fmt.Errorf("store: message %d: %w", id, err)
	}
	return r, nil
}

// MessagesToClassify returns unclassified messages, oldest first.
func (s *Store) MessagesToClassify(ctx context.Context, limit int) ([]MessageRow, error) {
	return queryMessages(ctx, s.db, `WHERE classified_at IS NULL ORDER BY received_at, id LIMIT ?`, limit)
}

// UpdateMessageFlags persists the header-derived flags.
func (s *Store) UpdateMessageFlags(ctx context.Context, messageID int64, auto, bulk, list bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE message SET is_auto = ?, is_bulk = ?, is_list = ? WHERE id = ?`, auto, bulk, list, messageID)
	if err != nil {
		return fmt.Errorf("store: update flags: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("store: update flags: message %d not found", messageID)
	}
	return nil
}

// PurgeBodies nulls bodies of messages received before the cutoff, keeps
// every other column, and logs the purge. Returns the number purged.
func (s *Store) PurgeBodies(ctx context.Context, before, now time.Time) (int64, error) {
	var n int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE message SET body_text = NULL, body_preview = NULL, body_purged_at = ?
			WHERE received_at < ? AND body_purged_at IS NULL`, ts(now), ts(before))
		if err != nil {
			return fmt.Errorf("store: purge bodies: %w", err)
		}
		n, _ = res.RowsAffected()
		return logActionTx(ctx, tx, "system", "purge_bodies", "message",
			map[string]any{"before": ts(before), "count": n}, now)
	})
	return n, err
}
