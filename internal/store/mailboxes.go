package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
)

// Mailbox runtime states.
const (
	StateUnknown  = "unknown"
	StateOK       = "ok"
	StateDegraded = "degraded"
)

// MailboxRow is one followed mailbox with its runtime state.
type MailboxRow struct {
	Address                 string
	Enabled, Digest         bool
	PollSeconds             int
	LastOKAt                *time.Time
	LastSyncCompletedAt     *time.Time
	SyncTruncated           bool
	State, Reason           string
	CursorInbox, CursorSent time.Time
}

// UpsertMailbox materializes a config entry. Only config-owned columns are
// written on conflict; runtime state, sync freshness and cursors survive.
func (s *Store) UpsertMailbox(ctx context.Context, mb config.MailboxConfig) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mailbox (address, enabled, digest, poll_interval_s) VALUES (?,?,?,?)
		ON CONFLICT(address) DO UPDATE SET enabled = excluded.enabled, digest = excluded.digest,
		poll_interval_s = excluded.poll_interval_s`, mb.Address, mb.Enabled, mb.Digest, mb.PollSeconds)
	if err != nil {
		return fmt.Errorf("store: upsert mailbox %s: %w", mb.Address, err)
	}
	return nil
}

// SetMailboxState records a probe/sync outcome. StateOK also stamps last_ok_at.
func (s *Store) SetMailboxState(ctx context.Context, address, state, reason string, now time.Time) error {
	q := `UPDATE mailbox SET state = ?, reason = ? WHERE address = ?`
	args := []any{state, reason, address}
	if state == StateOK {
		q = `UPDATE mailbox SET state = ?, reason = ?, last_ok_at = ? WHERE address = ?`
		args = []any{state, reason, ts(now), address}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: set mailbox state: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("store: set mailbox state: unknown mailbox %s", address)
	}
	return nil
}

// SetSyncResult records a successful poll pass. A truncated pass preserves
// the last completed sync timestamp until a later pass finishes uncapped.
func (s *Store) SetSyncResult(ctx context.Context, mailbox string, truncated bool, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE mailbox SET sync_truncated = ?,
		last_sync_completed_at = CASE WHEN ? THEN last_sync_completed_at ELSE ? END
		WHERE address = ?`, truncated, truncated, ts(now), mailbox)
	if err != nil {
		return fmt.Errorf("store: set sync result: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("store: set sync result: unknown mailbox %s", mailbox)
	}
	return nil
}

// ListMailboxes returns every known mailbox ordered by address.
func (s *Store) ListMailboxes(ctx context.Context) ([]MailboxRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT address, enabled, digest, poll_interval_s, last_ok_at, state, reason,
		cursor_inbox, cursor_sentitems, last_sync_completed_at, sync_truncated FROM mailbox ORDER BY address`)
	if err != nil {
		return nil, fmt.Errorf("store: list mailboxes: %w", err)
	}
	defer rows.Close()
	var out []MailboxRow
	for rows.Next() {
		var r MailboxRow
		var lastOK, ci, cs, lastSync sql.NullString
		if err := rows.Scan(&r.Address, &r.Enabled, &r.Digest, &r.PollSeconds, &lastOK, &r.State, &r.Reason,
			&ci, &cs, &lastSync, &r.SyncTruncated); err != nil {
			return nil, err
		}
		if r.LastOKAt, err = scanTS(lastOK); err != nil {
			return nil, err
		}
		if r.LastSyncCompletedAt, err = scanTS(lastSync); err != nil {
			return nil, err
		}
		for _, p := range []struct {
			src sql.NullString
			dst *time.Time
		}{{ci, &r.CursorInbox}, {cs, &r.CursorSent}} {
			t, err := scanTS(p.src)
			if err != nil {
				return nil, err
			}
			if t != nil {
				*p.dst = *t
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func cursorColumn(folder string) (string, error) {
	switch folder {
	case FolderInbox:
		return "cursor_inbox", nil
	case FolderSentItems:
		return "cursor_sentitems", nil
	}
	return "", fmt.Errorf("store: unsupported folder %q", folder)
}

// Cursor returns the folder's sync cursor; zero when never synced.
func (s *Store) Cursor(ctx context.Context, mailbox, folder string) (time.Time, error) {
	col, err := cursorColumn(folder)
	if err != nil {
		return time.Time{}, err
	}
	var v sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT `+col+` FROM mailbox WHERE address = ?`, mailbox).Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("store: cursor: %w", err)
	}
	t, err := scanTS(v)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}

// SetCursor stores the folder's sync cursor.
func (s *Store) SetCursor(ctx context.Context, mailbox, folder string, t time.Time) error {
	col, err := cursorColumn(folder)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE mailbox SET `+col+` = ? WHERE address = ?`, ts(t), mailbox)
	if err != nil {
		return fmt.Errorf("store: set cursor: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("store: set cursor: unknown mailbox %s", mailbox)
	}
	return nil
}

// DisableMailboxesExcept disables every mailbox whose address is not in keep.
func (s *Store) DisableMailboxesExcept(ctx context.Context, keep []string) error {
	args := make([]any, len(keep))
	for i, a := range keep {
		args[i] = a
	}
	q := `UPDATE mailbox SET enabled = 0`
	if len(keep) > 0 {
		q += ` WHERE address NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",") + `)`
	}
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("store: disable mailboxes: %w", err)
	}
	return nil
}
