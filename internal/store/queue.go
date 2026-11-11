package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxAttempts after which a failing job is dead-lettered (status 'failed').
const MaxAttempts = 5

const maxErrLen = 500

// Job is one claimed queue row.
type Job struct {
	ID, Ref  int64
	Kind     string
	Attempts int
}

// EnqueueTx adds a pending job inside the caller's transaction, so an ingest
// and its classify job commit or roll back together.
func (s *Store) EnqueueTx(ctx context.Context, tx *sql.Tx, kind string, ref int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO queue(kind, ref, status, attempts, not_before, created_at, updated_at)
		VALUES (?, ?, 'pending', 0, ?, ?, ?)`, kind, ref, ts(now), ts(now), ts(now))
	if err != nil {
		return fmt.Errorf("store: enqueue %s: %w", kind, err)
	}
	return nil
}

// Enqueue adds a pending job in its own transaction.
func (s *Store) Enqueue(ctx context.Context, kind string, ref int64, now time.Time) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error { return s.EnqueueTx(ctx, tx, kind, ref, now) })
}

// Claim atomically takes the oldest due pending job of the given kinds (any
// kind when empty) and marks it running. Nil when nothing is due.
func (s *Store) Claim(ctx context.Context, kinds []string, now time.Time) (*Job, error) {
	filter, args := "", []any{ts(now)}
	if len(kinds) > 0 {
		filter = ` AND kind IN (` + strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",") + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	var j *Job
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `UPDATE queue SET status = 'running', updated_at = ?
			WHERE id = (SELECT id FROM queue WHERE status = 'pending' AND not_before <= ?`+filter+`
			            ORDER BY not_before, id LIMIT 1)
			RETURNING id, kind, ref, attempts`, append([]any{ts(now)}, args...)...)
		var got Job
		if err := row.Scan(&got.ID, &got.Kind, &got.Ref, &got.Attempts); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("store: claim: %w", err)
		}
		j = &got
		return nil
	})
	return j, err
}

// Complete marks a job done.
func (s *Store) Complete(ctx context.Context, id int64, now time.Time) error {
	return s.setJob(ctx, `UPDATE queue SET status = 'done', updated_at = ? WHERE id = ?`, ts(now), id)
}

// Fail records an error: attempts+1, retry after 2^attempts minutes, and
// dead-letter ('failed') once attempts reaches MaxAttempts.
func (s *Store) Fail(ctx context.Context, id int64, errMsg string, now time.Time) error {
	if len(errMsg) > maxErrLen {
		errMsg = errMsg[:maxErrLen]
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var attempts int
		if err := tx.QueryRowContext(ctx, `SELECT attempts FROM queue WHERE id = ?`, id).Scan(&attempts); err != nil {
			return fmt.Errorf("store: fail job %d: %w", id, err)
		}
		attempts++
		status, notBefore := "pending", now.Add(time.Duration(1<<attempts)*time.Minute)
		if attempts >= MaxAttempts {
			status = "failed"
		}
		_, err := tx.ExecContext(ctx, `UPDATE queue SET status = ?, attempts = ?, not_before = ?, last_error = ?, updated_at = ?
			WHERE id = ?`, status, attempts, ts(notBefore), errMsg, ts(now), id)
		return err
	})
}

// Release puts a running job back to pending until notBefore WITHOUT
// counting an attempt (budget stops are not failures).
func (s *Store) Release(ctx context.Context, id int64, notBefore, now time.Time) error {
	return s.setJob(ctx, `UPDATE queue SET status = 'pending', not_before = ?, updated_at = ? WHERE id = ?`,
		ts(notBefore), ts(now), id)
}

// ResetRunning returns jobs left 'running' by a crashed process to pending.
// Call once at boot, before the pool starts.
func (s *Store) ResetRunning(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE queue SET status = 'pending', updated_at = ? WHERE status = 'running'`, ts(now))
	if err != nil {
		return 0, fmt.Errorf("store: reset running: %w", err)
	}
	return res.RowsAffected()
}

// QueueDepth reports backlog (pending + running), dead-letters, and the age
// in seconds of the oldest backlog job.
func (s *Store) QueueDepth(ctx context.Context, now time.Time) (pending, failed, oldestPendingSeconds int, err error) {
	var oldest sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT
		coalesce(sum(status IN ('pending','running')), 0),
		coalesce(sum(status = 'failed'), 0),
		min(CASE WHEN status IN ('pending','running') THEN created_at END)
		FROM queue`).Scan(&pending, &failed, &oldest)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: queue depth: %w", err)
	}
	if oldest.Valid {
		t, perr := parseTS(oldest.String)
		if perr != nil {
			return 0, 0, 0, perr
		}
		oldestPendingSeconds = int(now.Sub(t) / time.Second)
	}
	return pending, failed, oldestPendingSeconds, nil
}

// LatestJobStatus is the status of the newest job for (kind, ref); "" when
// there is none.
func (s *Store) LatestJobStatus(ctx context.Context, kind string, ref int64) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM queue WHERE kind = ? AND ref = ? ORDER BY id DESC LIMIT 1`,
		kind, ref).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: latest job: %w", err)
	}
	return status, nil
}

func (s *Store) setJob(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: update job: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("store: update job: not found")
	}
	return nil
}
