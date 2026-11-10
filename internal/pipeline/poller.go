package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/alessiosavi/GoGPUtils/textnorm"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

// Job kinds.
const (
	KindClassify      = "classify"
	KindThreadRefresh = "thread_refresh"
	KindDigest        = "digest"
)

// Folders read per mailbox: direction is a fact of the folder.
var syncFolders = []string{store.FolderInbox, store.FolderSentItems}

// Poller is the producer for one mailbox.
type Poller struct {
	c    *graph.Client
	s    *store.Store
	mb   config.MailboxConfig
	cfg  *config.Config
	wake chan<- struct{}
	// Top and MaxPages bound one folder read (0 = graph defaults).
	Top, MaxPages int
	log           *slog.Logger
}

// NewPoller builds the producer for one mailbox. wake may be nil.
func NewPoller(c *graph.Client, s *store.Store, mb config.MailboxConfig, cfg *config.Config, wake chan<- struct{}) *Poller {
	return &Poller{c: c, s: s, mb: mb, cfg: cfg, wake: wake,
		log: slog.Default().With("component", "poller", "mailbox", mb.Address)}
}

// Mailbox is the address this poller reads.
func (p *Poller) Mailbox() string { return p.mb.Address }

// SyncOnce reads inbox and sentitems since cursor−overlap (or now−initial
// days on a first run), stores each message and its classify job in one
// transaction, and advances a folder's cursor only after all of that
// folder's messages committed. Any error leaves the cursor where it was and
// marks the mailbox degraded; success marks it ok.
func (p *Poller) SyncOnce(ctx context.Context, now time.Time) (fetched, inserted int, err error) {
	for _, folder := range syncFolders {
		f, ins, err := p.syncFolder(ctx, folder, now)
		fetched, inserted = fetched+f, inserted+ins
		if err != nil {
			err = fmt.Errorf("sync %s/%s: %w", p.mb.Address, folder, err)
			if serr := p.s.SetMailboxState(ctx, p.mb.Address, store.StateDegraded, err.Error(), now); serr != nil {
				p.log.Error("record degraded state", "err", serr)
			}
			return fetched, inserted, err
		}
	}
	if err := p.s.SetMailboxState(ctx, p.mb.Address, store.StateOK, "", now); err != nil {
		return fetched, inserted, err
	}
	if inserted > 0 {
		p.log.Info("synced", "fetched", fetched, "inserted", inserted)
	}
	return fetched, inserted, nil
}

func (p *Poller) syncFolder(ctx context.Context, folder string, now time.Time) (fetched, inserted int, err error) {
	cursor, err := p.s.Cursor(ctx, p.mb.Address, folder)
	if err != nil {
		return 0, 0, err
	}
	since := cursor.Add(-time.Duration(p.cfg.PollOverlapMinutes) * time.Minute)
	if cursor.IsZero() {
		since = now.AddDate(0, 0, -p.cfg.InitialDays)
	}
	truncated := -1
	msgs, err := p.c.ListMessages(ctx, p.mb.Address, folder, since, graph.ListOptions{
		Top: p.Top, MaxPages: p.MaxPages, Truncated: func(n int) { truncated = n },
	})
	if err != nil {
		return 0, 0, err
	}
	newest := cursor
	for _, m := range msgs {
		var isNew bool
		err := p.s.WithTx(ctx, func(tx *sql.Tx) error {
			id, ins, err := p.s.UpsertMessageTx(ctx, tx, m, ThreadKey(m), now)
			if err != nil || !ins {
				return err
			}
			isNew = true
			return p.s.EnqueueTx(ctx, tx, KindClassify, id, now)
		})
		if err != nil {
			return len(msgs), inserted, err
		}
		if isNew {
			inserted++
			p.signal()
		}
		if m.ReceivedAt.After(newest) {
			newest = m.ReceivedAt
		}
	}
	if truncated >= 0 {
		p.log.Warn("sync truncated at page cap; next run continues from the newest fetched message",
			"folder", folder, "fetched", truncated)
		if err := p.s.LogAction(ctx, "poller", "sync_truncated", p.mb.Address+"/"+folder,
			map[string]any{"fetched": truncated, "since": since.UTC().Format(time.RFC3339)}, now); err != nil {
			return len(msgs), inserted, err
		}
	}
	if newest.After(cursor) {
		if err := p.s.SetCursor(ctx, p.mb.Address, folder, newest); err != nil {
			return len(msgs), inserted, err
		}
	}
	return len(msgs), inserted, nil
}

func (p *Poller) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// SyncAll runs every poller once. One mailbox failing never stops the
// others; the per-mailbox errors are joined.
func SyncAll(ctx context.Context, pollers []*Poller, now time.Time) error {
	var errs []error
	for _, p := range pollers {
		if _, _, err := p.SyncOnce(ctx, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var (
	replyPrefix = regexp.MustCompile(`(?i)^\s*((re|r|fw|fwd|i|aw|wg|tr|rif)\s*(\[\d+\])?\s*:\s*)+`)
	subjectNorm = textnorm.SearchPreset()
)

// ThreadKey groups messages into threads: Graph's conversationId when set,
// else the normalized subject plus the external counterpart (sender for
// inbound, first recipient for outbound).
func ThreadKey(m graph.Message) string {
	if m.ConversationID != "" {
		return "c:" + m.ConversationID
	}
	subj := NormalizeSubject(m.Subject)
	counterpart := m.From
	if m.Folder == store.FolderSentItems && len(m.To) > 0 {
		counterpart = m.To[0]
	}
	return "s:" + subj + "|" + strings.ToLower(counterpart)
}

// NormalizeSubject strips reply/forward prefixes and reduces a subject to
// lower-case letters, digits and single spaces.
func NormalizeSubject(subject string) string {
	subj, err := subjectNorm.Run(replyPrefix.ReplaceAllString(subject, ""))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(subject))
	}
	return subj
}
