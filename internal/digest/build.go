// Package digest builds, renders and sends the working-day digest and the
// immediate P0 alerts. User-facing text is Italian.
package digest

import (
	"context"
	"fmt"
	"time"

	"github.com/alessiosavi/GoGPUtils/sliceutil"
	"github.com/alessiosavi/GoGPUtils/stringutil"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

const (
	maxItems  = 25 // per section; the rest is counted
	reasonLen = 80
	runHour   = 8 // digest time, Europe/Rome
)

// Labels with special digest handling.
const (
	labelNotifications = "notifications"
	labelPersonal      = "personal"
)

// Draft is one rendered digest.
type Draft struct {
	Day      string
	Subject  string
	Markdown string
	HTML     string
	Health   Health
}

// Health is the system state printed in every digest (and stored as JSON).
type Health struct {
	Degraded             []Degraded `json:"degraded"`
	QueuePending         int        `json:"queue_pending"`
	QueueFailed          int        `json:"queue_failed"`
	OldestPendingSeconds int        `json:"oldest_pending_seconds"`
	Unclassified         int        `json:"unclassified"`
	TokensUsed           int        `json:"tokens_used"`
	TokensLimit          int        `json:"tokens_limit"`
	BudgetReached        bool       `json:"budget_reached"`
}

// Degraded is one mailbox with a problem.
type Degraded struct {
	Address string `json:"address"`
	Reason  string `json:"reason"`
}

type item struct {
	ID                                  int64
	From, Subject, Mailbox, Age, Reason string
}

type section struct {
	Title       string
	Count, More int
	Items       []item
}

type replyRow struct {
	ID                            int64
	Label, From, Subject, Mailbox string
	Waiting, Threshold            string
	Overdue                       bool
}

type view struct {
	DateLong, WindowFrom, WindowTo string
	NeedsReply                     []replyRow
	Sections                       []section
	Unsure                         []item
	Notifications                  int
	Health                         Health
	OldestMin, BudgetPct           int
}

// WindowStart is the previous working day at 08:00 Europe/Rome before at
// (Monday's digest covers the weekend from Friday 08:00).
func WindowStart(at time.Time) time.Time {
	d := at.In(config.Rome)
	start := time.Date(d.Year(), d.Month(), d.Day(), runHour, 0, 0, 0, config.Rome)
	if !start.Before(d) {
		start = start.AddDate(0, 0, -1)
	}
	for start.Weekday() == time.Saturday || start.Weekday() == time.Sunday {
		start = start.AddDate(0, 0, -1)
	}
	return start.UTC()
}

// Build assembles the digest for the instant at (normally 08:00 Rome of a
// working day). Run threads.Sweep first so reply states are current.
func Build(ctx context.Context, s *store.Store, cfg *config.Config, t *classify.Taxonomy, at time.Time) (*Draft, error) {
	from := WindowStart(at)
	// The current config decides which mailboxes are in the digest (the DB
	// copy of the flags can be stale for one-shot commands).
	digestOn := map[string]bool{}
	for _, mb := range cfg.Mailboxes {
		digestOn[mb.Address] = mb.Enabled && mb.Digest
	}
	all, err := s.DigestMessages(ctx, from, at)
	if err != nil {
		return nil, err
	}
	msgs := sliceutil.Filter(all, func(d store.DigestItem) bool { return digestOn[d.Mailbox] })
	v := view{DateLong: longDate(at), WindowFrom: shortStamp(from), WindowTo: shortStamp(at)}

	groups := sliceutil.GroupBy(msgs, func(d store.DigestItem) string { return d.Label })
	for _, key := range t.Order {
		ms := groups[key]
		switch key {
		case labelNotifications:
			v.Notifications = len(ms)
			continue
		case labelPersonal:
			continue
		case classify.Unsure:
			for _, m := range ms {
				v.Unsure = append(v.Unsure, toItem(m, at))
			}
			continue
		}
		if len(ms) == 0 {
			continue
		}
		sec := section{Title: t.Title(key), Count: len(ms)}
		for _, m := range sliceutil.Take(ms, maxItems) {
			sec.Items = append(sec.Items, toItem(m, at))
		}
		sec.More = len(ms) - len(sec.Items)
		v.Sections = append(v.Sections, sec)
	}

	open, err := s.ListNeedsReply(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range open {
		if !digestOn[r.Mailbox] || r.LastInboundAt == nil || r.Cassetto == labelPersonal {
			continue
		}
		row := replyRow{ID: r.LastInboundMessageID, Label: t.Title(r.Cassetto), From: r.LastInboundFrom, Subject: r.Subject, Mailbox: r.Mailbox,
			Waiting: age(at.Sub(*r.LastInboundAt))}
		if r.Priority != "" {
			row.Label = r.Priority + " · " + row.Label
		}
		if r.ReplyDueAt != nil {
			th := r.ReplyDueAt.Sub(*r.LastInboundAt)
			row.Threshold = fmt.Sprintf("%dh", int(th.Hours()))
			row.Overdue = !at.Before(*r.ReplyDueAt)
		}
		v.NeedsReply = append(v.NeedsReply, row)
	}

	if v.Health, err = health(ctx, s, cfg, at); err != nil {
		return nil, err
	}
	v.OldestMin = v.Health.OldestPendingSeconds / 60
	if v.Health.TokensLimit > 0 {
		v.BudgetPct = min(100, v.Health.TokensUsed*100/v.Health.TokensLimit)
	}
	md, html, err := render(v)
	if err != nil {
		return nil, err
	}
	subject := "Riepilogo email — " + v.DateLong
	if n := len(v.NeedsReply); n > 0 {
		subject += fmt.Sprintf(" · %d da rispondere", n)
	}
	return &Draft{Day: config.Day(at), Subject: subject, Markdown: md, HTML: html, Health: v.Health}, nil
}

func health(ctx context.Context, s *store.Store, cfg *config.Config, at time.Time) (Health, error) {
	h := Health{TokensLimit: cfg.LLM.DailyMaxTokens, Degraded: []Degraded{}}
	boxes, err := s.ListMailboxes(ctx)
	if err != nil {
		return h, err
	}
	for _, b := range boxes {
		if b.Enabled && b.State == store.StateDegraded {
			h.Degraded = append(h.Degraded, Degraded{Address: b.Address, Reason: stringutil.Truncate(b.Reason, 160, "…")})
		}
	}
	if h.QueuePending, h.QueueFailed, h.OldestPendingSeconds, err = s.QueueDepth(ctx, at); err != nil {
		return h, err
	}
	if h.Unclassified, err = s.UnclassifiedCount(ctx); err != nil {
		return h, err
	}
	in, out, err := s.UsageToday(ctx, config.Day(at))
	if err != nil {
		return h, err
	}
	h.TokensUsed = in + out
	h.BudgetReached = h.TokensLimit > 0 && h.TokensUsed >= h.TokensLimit
	return h, nil
}

func toItem(m store.DigestItem, at time.Time) item {
	from := m.From
	if m.FromName != "" {
		from = m.FromName + " (" + m.From + ")"
	}
	return item{ID: m.ID, From: from, Subject: m.Subject, Mailbox: m.Mailbox, Age: age(at.Sub(m.ReceivedAt)),
		Reason: stringutil.Truncate(m.Rationale, reasonLen, "…")}
}

// age renders a duration in Italian: minutes, hours up to 72h, then days.
func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return plural(int(d/time.Minute), "minuto", "minuti")
	case d < 72*time.Hour:
		return plural(int(d/time.Hour), "ora", "ore")
	}
	return plural(int(d/(24*time.Hour)), "giorno", "giorni")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

var (
	weekdays = [...]string{"domenica", "lunedì", "martedì", "mercoledì", "giovedì", "venerdì", "sabato"}
	months   = [...]string{"gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno", "luglio", "agosto",
		"settembre", "ottobre", "novembre", "dicembre"}
)

func longDate(t time.Time) string {
	d := t.In(config.Rome)
	return fmt.Sprintf("%s %d %s %d", weekdays[d.Weekday()], d.Day(), months[d.Month()-1], d.Year())
}

func shortStamp(t time.Time) string {
	d := t.In(config.Rome)
	return fmt.Sprintf("%s %d %s %02d:%02d", weekdays[d.Weekday()][:3], d.Day(), months[d.Month()-1][:3], d.Hour(), d.Minute())
}
