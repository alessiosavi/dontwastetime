package store

import (
	"context"
	"database/sql"
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
		method, model, rationale, cost_tokens, classified_at, taxonomy_version) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(message_id) DO UPDATE SET label = excluded.label, entity = excluded.entity,
		confidence = excluded.confidence, urgent = excluded.urgent, negative = excluded.negative, method = excluded.method,
		model = excluded.model, rationale = excluded.rationale, cost_tokens = excluded.cost_tokens,
		classified_at = excluded.classified_at, taxonomy_version = excluded.taxonomy_version`,
		c.MessageID, c.Label, c.Entity, c.Confidence, c.Urgent, c.Negative, c.Method, c.Model, c.Rationale,
		c.CostTokens, ts(now), c.TaxonomyVersion)
	if err != nil {
		return fmt.Errorf("store: save classification %d: %w", c.MessageID, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE message SET classified_at = ?, classify_lease = NULL WHERE id = ?`, ts(now), c.MessageID)
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
		cost_tokens, taxonomy_version FROM classification WHERE message_id = ?`, messageID).Scan(
		&c.Label, &c.Entity, &c.Confidence, &c.Urgent, &c.Negative, &c.Method, &c.Model, &c.Rationale,
		&c.CostTokens, &c.TaxonomyVersion)
	if err != nil {
		return c, fmt.Errorf("store: classification %d: %w", messageID, err)
	}
	return c, nil
}

// AddCorrection records a human relabel, makes it the message's
// classification (method 'correction', confidence 1) and logs it, in one
// transaction.
func (s *Store) AddCorrection(ctx context.Context, messageID int64, oldLabel, newLabel, by string, now time.Time) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO corrections(message_id, old_label, new_label, by, at) VALUES (?,?,?,?,?)`,
			messageID, oldLabel, newLabel, by, ts(now)); err != nil {
			return fmt.Errorf("store: add correction: %w", err)
		}
		if err := SaveClassificationTx(ctx, tx, Classification{MessageID: messageID, Label: newLabel, Confidence: 1,
			Method: "correction", Rationale: "corrected by " + by}, now); err != nil {
			return err
		}
		return logActionTx(ctx, tx, by, "correct", "message:"+strconv.FormatInt(messageID, 10),
			map[string]any{"old": oldLabel, "new": newLabel}, now)
	})
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
		res, err := tx.ExecContext(ctx, `UPDATE thread SET muted_at = ?, muted_by = ?, needs_reply = 0 WHERE thread_key = ?`,
			ts(now), by, threadKey)
		if err != nil {
			return fmt.Errorf("store: mute thread: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("store: mute thread: unknown thread %q", threadKey)
		}
		return logActionTx(ctx, tx, by, "mute_thread", "thread:"+threadKey, nil, now)
	})
}
