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
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
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
)

// thresholds: labels that expect a reply. Every other label: never.
var thresholds = map[string]time.Duration{
	"clienti": 24 * time.Hour, "business.dev": 24 * time.Hour,
	"ops.spaces": 48 * time.Hour, "eventi.internal": 48 * time.Hour, "eventi.external": 48 * time.Hour,
	"eventi.post": 48 * time.Hour, "admin.requests": 48 * time.Hour,
}

var (
	urgencyRe  = regexp.MustCompile(`(?i)\b(urgente|urgent|entro|asap|scadenza|bloccante)\b`)
	questionRe = regexp.MustCompile(`(?i)\?|\b(rinnov\w*|contratt\w*|offert\w*|preventiv\w*|propost\w*|riunione|meeting|call|possiamo|potete|potresti|quando)\b`)
	pecRe      = regexp.MustCompile(`(?i)^\s*(accettazione|avvenuta consegna|mancata consegna|posta certificata|anomalia)\b`)
	calendarRe = regexp.MustCompile(`(?i)^\s*(accettato|accepted|rifiutato|declined|provvisorio|tentative|tentativo|invito aggiornato|updated invitation)\s*:`)
	oofRe      = regexp.MustCompile(`(?i)^\s*(risposta automatica|automatic reply|auto(matic)?[ -]?reply|out of office|fuori ufficio|undeliverable|non recapitabile)`)
	noReplyRe  = regexp.MustCompile(`(?i)^(no-?reply|do-?not-?reply|mailer-daemon|postmaster|notifications?|newsletter|bounces?)\b`)
)

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
	msgs, err := s.ThreadMessages(ctx, key)
	if err != nil {
		return Status{}, err
	}
	if len(msgs) == 0 {
		return Status{}, fmt.Errorf("threads: %q has no messages", key)
	}
	internal := internalDomains(cfg)
	var lastIn, lastOurs *store.ThreadMessage
	cassetto := ""
	for i := range msgs {
		m := &msgs[i]
		if m.Direction == "out" || internal[domain(m.From)] {
			lastOurs = m
		} else {
			lastIn = m
		}
		if m.Label != "" {
			cassetto = m.Label
		}
	}
	if lastIn != nil && lastIn.Label != "" {
		cassetto = lastIn.Label
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
	open := lastIn != nil && (lastOurs == nil || lastOurs.ReceivedAt.Before(lastIn.ReceivedAt)) &&
		!suppressed(lastIn) && row.MutedAt == nil
	if open && tracked {
		age := now.Sub(lastIn.ReceivedAt)
		st.DueAt = lastIn.ReceivedAt.Add(threshold)
		if cassetto == "clienti" {
			known, err := knownClient(ctx, s, cfg, domain(lastIn.From), key)
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

func suppressed(m *store.ThreadMessage) bool {
	local, _, _ := strings.Cut(m.From, "@")
	return m.IsAuto || m.IsBulk || m.IsList || noReplyRe.MatchString(local) ||
		pecRe.MatchString(m.Subject) || calendarRe.MatchString(m.Subject) || oofRe.MatchString(m.Subject)
}

func knownClient(ctx context.Context, s *store.Store, cfg *config.Config, dom, exceptThread string) (bool, error) {
	if dom == "" {
		return false, nil
	}
	for _, d := range cfg.TopClients {
		if dom == d || strings.HasSuffix(dom, "."+d) {
			return true, nil
		}
	}
	return s.KnownClientDomain(ctx, dom, exceptThread)
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
	if row.P0AlertedDay == day || !digestMailbox(cfg, row.Mailbox) {
		return nil
	}
	if err := alert(ctx, []store.ThreadRow{row}); err != nil {
		return fmt.Errorf("threads: P0 alert for %q: %w", key, err)
	}
	return s.MarkP0Alerted(ctx, key, day)
}

func digestMailbox(cfg *config.Config, address string) bool {
	for _, mb := range cfg.Mailboxes {
		if mb.Address == address {
			return mb.Digest
		}
	}
	return false
}
