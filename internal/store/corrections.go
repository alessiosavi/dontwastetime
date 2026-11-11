package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Classification is one message's label.
type Classification struct {
	MessageID       int64
	Label, Entity   string
	Confidence      float64
	Urgent          bool
	Negative        bool
	ReplyExpected   *bool  // NULL means use the label's legacy reply policy
	Method          string // rule | llm | correction
	Model           string
	Rationale       string
	CostTokens      int
	TaxonomyVersion string
}

// Correction is one human relabel, joined with the message fields the
// classifier uses as a few-shot example.
type Correction struct {
	MessageID          int64
	OldLabel, NewLabel string
	By                 string
	At                 time.Time
	Subject, From      string
	Preview            string
}

// SaveClassification stores c and marks the message classified (lease
// cleared) in one transaction.
func (s *Store) SaveClassification(ctx context.Context, c Classification, now time.Time) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error { return SaveClassificationTx(ctx, tx, c, now) })
}

// SaveClassificationTx is SaveClassification inside the caller's transaction.
func SaveClassificationTx(ctx context.Context, tx *sql.Tx, c Classification, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO classification(message_id, label, entity, confidence, urgent, negative,
		method, model, rationale, cost_tokens, classified_at, taxonomy_version, reply_expected) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(message_id) DO UPDATE SET label = excluded.label, entity = excluded.entity,
		confidence = excluded.confidence,
		urgent = CASE WHEN excluded.method = 'correction' THEN classification.urgent ELSE excluded.urgent END,
		negative = CASE WHEN excluded.method = 'correction' THEN classification.negative ELSE excluded.negative END,
		method = excluded.method,
		model = excluded.model, rationale = excluded.rationale, cost_tokens = excluded.cost_tokens,
		classified_at = excluded.classified_at, taxonomy_version = excluded.taxonomy_version,
		reply_expected = CASE WHEN excluded.method = 'correction' THEN classification.reply_expected ELSE excluded.reply_expected END
		WHERE classification.method <> 'correction' OR excluded.method = 'correction'`,
		c.MessageID, c.Label, c.Entity, c.Confidence, c.Urgent, c.Negative, c.Method, c.Model, c.Rationale,
		c.CostTokens, ts(now), c.TaxonomyVersion, c.ReplyExpected)
	if err != nil {
		return fmt.Errorf("store: save classification %d: %w", c.MessageID, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE message SET classified_at = ?, first_classified_at = coalesce(first_classified_at, ?),
		classify_lease = NULL WHERE id = ?`, ts(now), ts(now), c.MessageID)
	if err != nil {
		return fmt.Errorf("store: mark classified %d: %w", c.MessageID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("store: mark classified: message %d not found", c.MessageID)
	}
	return nil
}

// ClassificationOf loads one message's classification.
func (s *Store) ClassificationOf(ctx context.Context, messageID int64) (Classification, error) {
	c := Classification{MessageID: messageID}
	err := s.db.QueryRowContext(ctx, `SELECT label, entity, confidence, urgent, negative, method, model, rationale,
		cost_tokens, taxonomy_version, reply_expected FROM classification WHERE message_id = ?`, messageID).Scan(
		&c.Label, &c.Entity, &c.Confidence, &c.Urgent, &c.Negative, &c.Method, &c.Model, &c.Rationale,
		&c.CostTokens, &c.TaxonomyVersion, &c.ReplyExpected)
	if err != nil {
		return c, fmt.Errorf("store: classification %d: %w", messageID, err)
	}
	return c, nil
}

// AddCorrection reads and returns the old label, records a human relabel,
// makes it the message's classification (method 'correction', confidence 1),
// logs it and enqueues a thread refresh, all in one transaction.
func (s *Store) AddCorrection(ctx context.Context, messageID int64, newLabel, by string, now time.Time) (oldLabel string, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		oldLabel, err = s.AddCorrectionTx(ctx, tx, messageID, newLabel, by, now)
		return err
	})
	return oldLabel, err
}

// AddCorrectionTx is AddCorrection inside the caller's transaction.
func (s *Store) AddCorrectionTx(ctx context.Context, tx *sql.Tx, messageID int64, newLabel, by string, now time.Time) (oldLabel string, err error) {
	if err := tx.QueryRowContext(ctx, `SELECT label FROM classification WHERE message_id = ?`, messageID).Scan(&oldLabel); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: correction old label %d: %w", messageID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO corrections(message_id, old_label, new_label, by, at) VALUES (?,?,?,?,?)`,
		messageID, oldLabel, newLabel, by, ts(now)); err != nil {
		return oldLabel, fmt.Errorf("store: add correction: %w", err)
	}
	if err := SaveClassificationTx(ctx, tx, Classification{MessageID: messageID, Label: newLabel, Confidence: 1,
		Method: "correction", Rationale: "corrected by " + by}, now); err != nil {
		return oldLabel, err
	}
	if err := logActionTx(ctx, tx, by, "correct", "message:"+strconv.FormatInt(messageID, 10),
		map[string]any{"old": oldLabel, "new": newLabel}, now); err != nil {
		return oldLabel, err
	}
	return oldLabel, s.EnqueueTx(ctx, tx, KindThreadRefresh, messageID, now)
}

// RecentCorrections returns the newest n corrections with message context.
func (s *Store) RecentCorrections(ctx context.Context, n int) ([]Correction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.message_id, c.old_label, c.new_label, c.by, c.at, m.subject, m.from_addr,
		coalesce(m.body_preview, '') FROM corrections c JOIN message m ON m.id = c.message_id ORDER BY c.at DESC, c.id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("store: recent corrections: %w", err)
	}
	defer rows.Close()
	var out []Correction
	for rows.Next() {
		var c Correction
		var at string
		if err := rows.Scan(&c.MessageID, &c.OldLabel, &c.NewLabel, &c.By, &at, &c.Subject, &c.From, &c.Preview); err != nil {
			return nil, err
		}
		if c.At, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MuteThread silences a thread's needs-reply flag (resolved off-mail).
func (s *Store) MuteThread(ctx context.Context, threadKey, by string, now time.Time) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		return MuteThreadTx(ctx, tx, threadKey, by, now)
	})
}

// MuteThreadTx is MuteThread inside the caller's transaction.
func MuteThreadTx(ctx context.Context, tx *sql.Tx, key, by string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE thread SET muted_at = ?, muted_by = ?, needs_reply = 0 WHERE thread_key = ?`,
		ts(now), by, key)
	if err := threadUpdated(res, err, key); err != nil {
		return err
	}
	return logActionTx(ctx, tx, by, "mute_thread", "thread:"+key, nil, now)
}
