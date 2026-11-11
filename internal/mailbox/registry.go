// Package mailbox keeps the followed-mailbox registry: config → store sync,
// read probes with per-mailbox degradation, probe backoff, and the optional
// /users discovery helper.
package mailbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

// ErrDiscoveryForbidden: the credential cannot list users (no
// User.Read.All). Discovery is optional; the explicit config list stands.
var ErrDiscoveryForbidden = errors.New("mailbox discovery forbidden: credential lacks User.Read.All; using the explicit config list")

// probeCap bounds the probe backoff.
const probeCap = time.Hour

// State is one mailbox's verification outcome.
type State struct {
	Address   string
	OK        bool
	Reason    string
	NotBefore time.Time
}

// Verify probes inbox and sentitems with a one-message read.
func Verify(ctx context.Context, c *graph.Client, address string) error {
	for _, folder := range []string{store.FolderInbox, store.FolderSentItems} {
		if err := c.ProbeRead(ctx, address, folder); err != nil {
			return fmt.Errorf("mailbox %s (%s): %w", address, folder, err)
		}
	}
	return nil
}

// VerifyAll probes every enabled mailbox in the store, persists each outcome
// (ok / degraded with reason) and returns the states. One mailbox failing
// never stops the others.
func VerifyAll(ctx context.Context, c *graph.Client, s *store.Store, now time.Time) []State {
	rows, err := s.ListMailboxes(ctx)
	if err != nil {
		return []State{{Reason: err.Error()}}
	}
	var out []State
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		st := State{Address: r.Address, OK: true}
		if err := Verify(ctx, c, r.Address); err != nil {
			st.OK, st.Reason = false, err.Error()
			st.NotBefore = NextProbeTime(now, 1)
			_ = s.SetMailboxState(ctx, r.Address, store.StateDegraded, st.Reason, now)
		} else {
			_ = s.SetMailboxState(ctx, r.Address, store.StateOK, "", now)
		}
		out = append(out, st)
	}
	return out
}

// NextProbeTime is the earliest retry after `failures` consecutive failures:
// 1m, 2m, 4m … capped at 1h. Zero failures → now.
func NextProbeTime(now time.Time, failures int) time.Time {
	if failures <= 0 {
		return now
	}
	d := probeCap
	if failures <= 7 { // 2^6 minutes already exceeds the cap
		d = min(time.Duration(1<<(failures-1))*time.Minute, probeCap)
	}
	return now.Add(d)
}

// SyncFromConfig materializes the config's mailboxes into the store
// (runtime columns preserved) and disables rows no longer in the config:
// config is authoritative.
func SyncFromConfig(ctx context.Context, s *store.Store, cfg *config.Config) error {
	keep := make([]string, 0, len(cfg.Mailboxes))
	for _, mb := range cfg.Mailboxes {
		if err := s.UpsertMailbox(ctx, mb); err != nil {
			return err
		}
		keep = append(keep, mb.Address)
	}
	return s.DisableMailboxesExcept(ctx, keep)
}

// Discover lists tenant user mailboxes (enabled accounts with a mail
// address) and reports which expected addresses were not found. Both lists
// are lower-cased and sorted. On 403 it returns ErrDiscoveryForbidden.
func Discover(ctx context.Context, c *graph.Client, expected []string) (found, missing []string, err error) {
	users, err := c.ListUsers(ctx)
	if err != nil {
		if errors.Is(err, graph.ErrForbidden) {
			return nil, nil, ErrDiscoveryForbidden
		}
		return nil, nil, fmt.Errorf("mailbox discovery: %w", err)
	}
	have := map[string]bool{}
	for _, u := range users {
		if u.AccountEnabled && u.Mail != "" {
			have[strings.ToLower(u.Mail)] = true
		}
	}
	for a := range have {
		found = append(found, a)
	}
	for _, e := range expected {
		if e = strings.ToLower(e); !have[e] {
			missing = append(missing, e)
		}
	}
	slices.Sort(found)
	slices.Sort(missing)
	return found, slices.Compact(missing), nil
}
