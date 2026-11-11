// Package threads computes each thread's reply state: who spoke
// last, whether a reply is owed and by when, the Clienti priority, and the
// P0 alert seam.
package threads

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

// Priorities. PriorityPinned marks client threads unanswered > 7 days.
const (
	PriorityP0     = "P0"
	PriorityP1     = "P1"
	PriorityP2     = "P2"
	PriorityPinned = "P1+"
)

const (
	clientSLA   = 24 * time.Hour
	pinAfter    = 7 * 24 * time.Hour
	sweepWindow = 30 * 24 * time.Hour
	// Backfills and long outages must not open reply obligations for stale mail.
	backlogAge = 14 * 24 * time.Hour
)

// thresholds supplies label SLAs and the reply fallback for NULL decisions.
var thresholds = map[string]time.Duration{
	"clienti": 24 * time.Hour, "business.dev": 24 * time.Hour,
	"ops.spaces": 48 * time.Hour, "eventi.internal": 48 * time.Hour, "eventi.external": 48 * time.Hour,
	"eventi.post": 48 * time.Hour, "admin.requests": 48 * time.Hour,
}

// neverReplyLabels protects personal mail's privacy and keeps count-only or
// automated categories from opening obligations, even with an explicit LLM true.
var neverReplyLabels = map[string]struct{}{
	"personal": {}, "notifications": {}, "marketing.news": {}, "tech.alerts": {},
}

var (
	urgencyRe  = regexp.MustCompile(`(?i)\b(urgente|urgent|entro|asap|scadenza|bloccante)\b`)
	questionRe = regexp.MustCompile(`(?i)\?|\b(rinnov\w*|contratt\w*|offert\w*|preventiv\w*|propost\w*|riunione|meeting|call|possiamo|potete|potresti|quando)\b`)
	pecRe      = regexp.MustCompile(`(?i)^\s*(accettazione|avvenuta consegna|mancata consegna|posta certificata|anomalia)\b`)
	calendarRe = regexp.MustCompile(`(?i)^\s*(accettato|accepted|rifiutato|declined|provvisorio|tentative|tentativo|invito aggiornato|updated invitation)\s*:`)
	oofRe      = regexp.MustCompile(`(?i)^\s*(risposta automatica|automatic reply|auto(matic)?[ -]?reply|out of office|fuori ufficio|undeliverable|non recapitabile)`)
	noReplyRe  = regexp.MustCompile(`(?i)^(no-?reply|do-?not-?reply|mailer-daemon|postmaster|notifications?|newsletter|bounces?)\b`)
)

// beforeSave is a test seam: when set, Recompute calls it between reading a
// thread's messages and saving its state. Always nil in production.
var beforeSave func()

// Status is a thread's computed reply state.
type Status struct {
	NeedsReply bool
	DueAt      time.Time
	Priority   string
	Cassetto   string
}

// Alerter sends a P0 alert for the given threads.
type Alerter func(ctx context.Context, rows []store.ThreadRow) error

// Recompute derives a thread's state from all its messages and stores it.
// "Ours" = sent from any tracked mailbox, or inbound from one of our own
// domains (a colleague's answer seen in cc).
func Recompute(ctx context.Context, s *store.Store, key string, cfg *config.Config, now time.Time) (Status, error) {
	unlock := lockThread(key)
	defer unlock()
	msgs, err := s.ThreadMessages(ctx, key)
	if err != nil {
		return Status{}, err
	}
	if len(msgs) == 0 {
		return Status{}, fmt.Errorf("threads: %q has no messages", key)
	}
	internal := internalDomains(cfg)
	ours := func(m *store.ThreadMessage) bool { return m.Direction == "out" || internal[domain(m.From)] }
	var lastIn, lastOurs *store.ThreadMessage
	cassetto := ""
	for i := range msgs {
		m := &msgs[i]
		if ours(m) {
			lastOurs = m
		} else {
			lastIn = m
		}
		if m.Label != "" {
			cassetto = m.Label
		}
	}
	if lastIn != nil {
		// Only our messages that address the client's domain are replies:
		// an internal-only discussion in the same conversation is not.
		lastOurs = nil
		for i := range msgs {
			if m := &msgs[i]; ours(m) && addresses(m, domain(lastIn.From)) {
				lastOurs = m
			}
		}
		if lastIn.Label != "" {
			cassetto = lastIn.Label
		}
	}
	row, err := s.Thread(ctx, key)
	if err != nil {
		row = store.ThreadRow{ThreadKey: key}
	}
	latest := msgs[len(msgs)-1]
	row.Subject, row.Mailbox, row.Cassetto = latest.Subject, latest.Mailbox, cassetto
	row.LastInboundAt, row.LastOutboundAt, row.LastInboundFrom, row.LastInboundMessageID = nil, nil, "", 0
	if lastIn != nil {
		at := lastIn.ReceivedAt
		row.LastInboundAt, row.LastInboundFrom, row.LastInboundMessageID, row.Mailbox = &at, lastIn.From, lastIn.ID, lastIn.Mailbox
	}
	if lastOurs != nil {
		at := lastOurs.ReceivedAt
		row.LastOutboundAt = &at
	}

	st := Status{Cassetto: cassetto}
	threshold, tracked := thresholds[cassetto]
	if !tracked {
		threshold = 48 * time.Hour
	}
	open := lastIn != nil && (lastOurs == nil || lastOurs.ReceivedAt.Before(lastIn.ReceivedAt)) &&
		lastIn.InsertedAt.Sub(lastIn.ReceivedAt) <= backlogAge && row.MutedAt == nil
	if open && ReplyDecision(cassetto, lastIn) {
		age := now.Sub(lastIn.ReceivedAt)
		st.DueAt = lastIn.ReceivedAt.Add(threshold)
		if cassetto == "clienti" {
			known, err := knownClient(ctx, s, cfg, lastIn)
			if err != nil {
				return st, err
			}
			st.Priority = priority(lastIn, known, age)
		}
		st.NeedsReply = age >= threshold || st.Priority == PriorityP0
	}
	row.NeedsReply, row.Priority, row.ReplyDueAt = st.NeedsReply, st.Priority, nil
	if !st.DueAt.IsZero() {
		due := st.DueAt
		row.ReplyDueAt = &due
	}
	if hook := beforeSave; hook != nil {
		hook()
	}
	return st, s.SaveThreadState(ctx, row, now)
}

// priority is the Clienti rubric for an unanswered inbound message.
func priority(m *store.ThreadMessage, known bool, age time.Duration) string {
	text := m.Subject + " " + m.Preview
	urgent := m.Urgent || urgencyRe.MatchString(text)
	switch {
	case known && (urgent || m.Negative || age >= clientSLA):
		return PriorityP0
	case age > pinAfter:
		return PriorityPinned
	case urgent || m.Negative || questionRe.MatchString(text):
		return PriorityP1 // urgency on an unknown domain is at most P1
	}
	return PriorityP2
}

// ReplyDecision reports whether an unanswered thread with this latest inbound
// expects a reply. After suppression and label exclusions, urgent or negative
// clienti mail always expects a reply. Timing, backlog, mute and known-client
// policy are excluded so evaluation can measure the classification's reply decision.
func ReplyDecision(label string, m *store.ThreadMessage) bool {
	if m == nil || suppressed(m) {
		return false
	}
	if _, excluded := neverReplyLabels[label]; excluded {
		return false
	}
	if label == "clienti" && (m.Urgent || m.Negative || urgencyRe.MatchString(m.Subject+" "+m.Preview)) {
		return true
	}
	if m.ReplyExpected != nil {
		return *m.ReplyExpected
	}
	_, tracked := thresholds[label]
	return tracked
}

func suppressed(m *store.ThreadMessage) bool {
	local, _, _ := strings.Cut(m.From, "@")
	return m.IsAuto || m.IsBulk || m.IsList || noReplyRe.MatchString(local) ||
		pecRe.MatchString(m.Subject) || calendarRe.MatchString(m.Subject) || oofRe.MatchString(m.Subject)
}

// knownClient excludes domains of all configured mailboxes (even disabled ones)
// and their subdomains. Otherwise, a top-clients domain or a sender we addressed
// in an outbound message before m (any thread) is known. History matches exact
// domains, except for free-mail providers where only the exact sender counts.
func knownClient(ctx context.Context, s *store.Store, cfg *config.Config, m *store.ThreadMessage) (bool, error) {
	dom := domain(m.From)
	if dom == "" {
		return false, nil
	}
	for _, mb := range cfg.Mailboxes {
		d := domain(mb.Address)
		if dom == d || strings.HasSuffix(dom, "."+d) {
			return false, nil
		}
	}
	for _, d := range cfg.TopClients {
		if dom == d || strings.HasSuffix(dom, "."+d) {
			return true, nil
		}
	}
	recipient := dom
	switch dom {
	case "gmail.com", "googlemail.com", "outlook.com", "hotmail.com", "hotmail.it", "live.com", "live.it",
		"yahoo.com", "yahoo.it", "icloud.com", "me.com", "libero.it", "virgilio.it", "tiscali.it", "alice.it",
		"proton.me", "protonmail.com":
		recipient = m.From
	}
	return s.HasOutboundTo(ctx, recipient, m.ReceivedAt)
}

// addresses reports whether m was sent to someone at dom.
func addresses(m *store.ThreadMessage, dom string) bool {
	for _, a := range append(append([]string(nil), m.To...), m.CC...) {
		if domain(a) == dom {
			return true
		}
	}
	return false
}

// threadLocks serializes Recompute per thread so a stale snapshot can never
// overwrite a newer one (single-process daemon).
var threadLocks sync.Map

func lockThread(key string) func() {
	mu, _ := threadLocks.LoadOrStore(key, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

func internalDomains(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	for _, mb := range cfg.Mailboxes {
		out[domain(mb.Address)] = true
	}
	return out
}

func domain(addr string) string {
	_, d, _ := strings.Cut(strings.ToLower(addr), "@")
	return d
}

// RefreshHandler is the "thread_refresh" consumer (Ref = message row id).
// A P0 thread on a digest-enabled mailbox is alerted at most once per
// Europe/Rome day. alert may be nil (no alerts in one-shot commands).
func RefreshHandler(s *store.Store, cfg *config.Config, alert Alerter, now func() time.Time) func(context.Context, *store.Job) error {
	return func(ctx context.Context, job *store.Job) error {
		t := now()
		key, err := s.UpsertThreadFromMessage(ctx, job.Ref, t)
		if err != nil {
			return err
		}
		st, err := Recompute(ctx, s, key, cfg, t)
		if err != nil {
			return err
		}
		return maybeAlert(ctx, s, cfg, alert, key, st, t)
	}
}

// Sweep recomputes every open thread of the last 30 days, so time-driven
// changes (SLA breach, pinning) happen without a new message, and alerts
// new P0s. Run it periodically and before each digest.
func Sweep(ctx context.Context, s *store.Store, cfg *config.Config, alert Alerter, now time.Time) error {
	rows, err := s.OpenThreads(ctx, now.Add(-sweepWindow))
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		st, err := Recompute(ctx, s, r.ThreadKey, cfg, now)
		if err == nil {
			err = maybeAlert(ctx, s, cfg, alert, r.ThreadKey, st, now)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func maybeAlert(ctx context.Context, s *store.Store, cfg *config.Config, alert Alerter, key string, st Status, now time.Time) error {
	if alert == nil || st.Priority != PriorityP0 {
		return nil
	}
	row, err := s.Thread(ctx, key)
	if err != nil {
		return err
	}
	day := config.Day(now)
	if row.P0AlertedDay == day || !alertMailbox(cfg, row.Mailbox) {
		return nil
	}
	// Claim before sending: concurrent refreshes/sweeps of the same thread
	// must not both send. A rejected send gives the claim back; one that got
	// no answer keeps it (it may have been delivered).
	claimed, prev, err := s.ClaimP0Alert(ctx, key, day)
	if err != nil || !claimed {
		return err
	}
	row.P0AlertedDay = day
	if err := alert(ctx, []store.ThreadRow{row}); err != nil {
		if !errors.Is(err, graph.ErrDeliveryUnknown) {
			_ = s.UnclaimP0Alert(context.WithoutCancel(ctx), key, prev)
		}
		return fmt.Errorf("threads: P0 alert for %q: %w", key, err)
	}
	return nil
}

// alertMailbox: P0 alerts only for enabled mailboxes that are in the digest.
func alertMailbox(cfg *config.Config, address string) bool {
	for _, mb := range cfg.Mailboxes {
		if mb.Address == address {
			return mb.Enabled && mb.Digest
		}
	}
	return false
}
