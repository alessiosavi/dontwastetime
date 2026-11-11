// Package mailbox keeps the followed-mailbox registry: config → store sync,
// read probes with per-mailbox degradation, probe backoff, and the optional
// /users discovery helper.
package mailbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// Discover lists the tenant's readable mailboxes. Candidates are member
// accounts (guests, #EXT#, are skipped) addressed by mail, or by user
// principal name when mail is not visible to the credential; a candidate is
// kept only when its inbox is readable, and a candidate whose inbox is a
// configured address's (an alias of it) is not reported twice. found includes
// the readable configured addresses; missing lists configured addresses that
// are not readable (no such mailbox, or outside the access policy). A
// throttled or failed probe aborts: a partial list would read as complete.
// When the credential cannot list users it returns ErrDiscoveryForbidden.
func Discover(ctx context.Context, c *graph.Client, expected []string) (found, missing []string, err error) {
	users, err := c.ListUsers(ctx)
	if err != nil {
		if errors.Is(err, graph.ErrForbidden) {
			return nil, nil, ErrDiscoveryForbidden
		}
		return nil, nil, fmt.Errorf("mailbox discovery: %w", err)
	}
	probe := func(addr string) (id string, ok bool, err error) {
		id, err = c.InboxID(ctx, addr)
		switch {
		case err == nil:
			return id, true, nil
		case notAMailbox(err):
			return "", false, nil
		default:
			return "", false, fmt.Errorf("mailbox discovery: %s: %w", addr, err)
		}
	}
	seen, inConfig := map[string]bool{}, map[string]bool{} // inbox ids reported; configured addresses
	for _, e := range expected {
		if e = strings.ToLower(e); inConfig[e] {
			continue
		}
		inConfig[e] = true
		id, ok, err := probe(e)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			missing = append(missing, e)
			continue
		}
		seen[id] = true
		found = append(found, e)
	}
	var candidates []string
	for _, u := range users {
		if strings.Contains(u.UserPrincipalName, "#EXT#") {
			continue
		}
		addr := u.Mail
		if addr == "" {
			addr = u.UserPrincipalName
		}
		if addr = strings.ToLower(addr); addr != "" && !inConfig[addr] {
			candidates = append(candidates, addr)
		}
	}
	slices.Sort(candidates)
	for _, a := range slices.Compact(candidates) {
		id, ok, err := probe(a)
		if err != nil {
			return nil, nil, err
		}
		if ok && !seen[id] {
			seen[id] = true
			found = append(found, a)
		}
	}
	slices.Sort(found)
	slices.Sort(missing)
	return found, missing, nil
}

// notAMailbox: Graph answered that the address has no mailbox this credential
// can read (no such user or mailbox, or outside the application access policy).
func notAMailbox(err error) bool {
	var he *graph.HTTPError
	return errors.Is(err, graph.ErrForbidden) || (errors.As(err, &he) && he.Status == http.StatusNotFound)
}
