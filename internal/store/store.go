// Package store is dwt's single SQLite state file (WAL): mailboxes, messages,
// classifications, threads, the durable job queue, LLM usage, digests and the
// action log. Every multi-statement mutation is one BEGIN IMMEDIATE
// transaction (the DSN sets _txlock=immediate). Functions that read or write
// timestamps take the instant explicitly: the store never reads the clock.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no CGO
)

// ErrBudgetReached stops every LLM stage for the rest of the budget window.
// It lives here (not in pipeline or classify) so both can use it without an
// import cycle.
var ErrBudgetReached = errors.New("llm budget reached (daily tokens or monthly cost)")

// tsLayout is fixed-width UTC so lexical order equals time order in SQL.
const tsLayout = "2006-01-02T15:04:05.000Z"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(s string) (time.Time, error) { return time.Parse(tsLayout, s) }

// nullTS formats an optional instant.
func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

// scanTS converts a nullable column to *time.Time.
func scanTS(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTS(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Store wraps the database handle.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the state file and applies migrations.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read-only diagnostics (doctor) and tests.
func (s *Store) DB() *sql.DB { return s.db }

// WithTx runs fn in one BEGIN IMMEDIATE transaction, committing on nil.
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("store: schema_version: %w", err)
	}
	for i, ddl := range migrations {
		v := i + 1
		err := s.WithTx(ctx, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_version WHERE version = ?`, v).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, ddl); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES (?)`, v)
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migration %d: %w", v, err)
		}
	}
	return nil
}

// LogAction appends one row to action_log.
func (s *Store) LogAction(ctx context.Context, actor, action, target string, details map[string]any, now time.Time) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error { return logActionTx(ctx, tx, actor, action, target, details, now) })
}

func logActionTx(ctx context.Context, tx *sql.Tx, actor, action, target string, details map[string]any, now time.Time) error {
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("store: action details: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO action_log(at, actor, action, target, details_json) VALUES (?,?,?,?,?)`,
		ts(now), actor, action, target, string(raw))
	if err != nil {
		return fmt.Errorf("store: log action: %w", err)
	}
	return nil
}
