// Package service provides scoped, typed use cases for CLI and agent adapters.
// It never constructs network clients and receives its clock from the caller.
package service

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/digest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

var (
	ErrNotFound        = errors.New("non trovato")
	ErrStale           = errors.New("selezione non aggiornata")
	ErrInvalid         = errors.New("parametri non validi")
	ErrUnknownOperator = errors.New("operatore sconosciuto")
)

const unclassified = "non classificato"

type Options struct {
	Operator string
	CLI      bool
}

type Service struct {
	store      *store.Store
	cfg        *config.Config
	taxonomy   *classify.Taxonomy
	operator   string
	visibility store.Visibility
	now        func() time.Time
}

// New authorizes an operator and captures the enabled mailbox scope from config.
// CLI is reserved for adapters driven by a human at a terminal.
func New(s *store.Store, cfg *config.Config, tx *classify.Taxonomy, opt Options, now func() time.Time) (*Service, error) {
	if s == nil || cfg == nil || tx == nil || now == nil {
		return nil, invalid("store, configurazione, tassonomia e orologio richiesti")
	}
	operator := opt.Operator
	if opt.CLI {
		operator = "cli"
	} else if operator == "" || operator == "cli" || !slices.Contains(cfg.Operators, operator) {
		return nil, ErrUnknownOperator
	}
	svc := &Service{store: s, cfg: cfg, taxonomy: tx, operator: operator, now: now}
	for _, box := range cfg.Mailboxes {
		if box.Enabled && !slices.Contains(svc.visibility.Mailboxes, box.Address) {
			svc.visibility.Mailboxes = append(svc.visibility.Mailboxes, box.Address)
		}
	}
	return svc, nil
}

// storeError maps expected failures without putting store diagnostics or paths
// in the public sentinel errors. Unexpected errors remain available to adapters.
func storeError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case errors.Is(err, store.ErrStale):
		return ErrStale
	default:
		return err
	}
}

func (s *Service) meta(ctx context.Context, at time.Time) (Meta, error) {
	m := Meta{AsOf: at.UTC(), Freshness: []Freshness{}}
	boxes, err := s.store.ListMailboxes(ctx)
	if err != nil {
		return m, storeError(err)
	}
	byAddress := make(map[string]store.MailboxRow, len(boxes))
	for _, box := range boxes {
		byAddress[box.Address] = box
	}
	for _, address := range s.visibility.Mailboxes {
		box, ok := byAddress[address]
		if !ok {
			box.State = store.StateUnknown
		}
		m.Freshness = append(m.Freshness, Freshness{Mailbox: address,
			LastSyncCompletedAt: box.LastSyncCompletedAt, CatchingUp: box.SyncTruncated,
			State: box.State, Reason: box.Reason})
	}
	_, _, m.Unclassified, err = s.store.VisibleCounts(ctx, s.visibility, time.Time{}, at.Add(time.Millisecond))
	return m, storeError(err)
}

func (s *Service) bucket(key string) (string, string) {
	if key == "" {
		return unclassified, unclassified
	}
	return key, s.taxonomy.Title(key)
}

func (s *Service) validBucket(key string) error {
	if key != "" && key != unclassified && !s.taxonomy.Has(key) {
		return invalid("cassetto sconosciuto")
	}
	return nil
}

func (s *Service) item(m store.VisibleMessage, at time.Time) MessageItem {
	bucket, title := s.bucket(m.Label)
	preview := m.BodyText
	if m.BodyPurged || preview == "" {
		preview = m.BodyPreview
	}
	preview, trimmed := clip(preview, previewChars)
	return MessageItem{ID: m.ID, Thread: m.ThreadKey, Mailbox: m.Mailbox,
		Bucket: bucket, BucketTitle: title, ReceivedAt: m.ReceivedAt,
		Age: digest.Age(max(time.Duration(0), at.Sub(m.ReceivedAt))), Direction: m.Direction,
		HasAttachments: m.HasAttachments, BodyPurged: m.BodyPurged, Trimmed: trimmed,
		Untrusted: MailText{From: m.From, FromName: m.FromName, Subject: m.Subject, Preview: preview}}
}

func (s *Service) detailItem(m store.VisibleMessage, at time.Time, limit int) MessageItem {
	i := s.item(m, at)
	i.Trimmed = false
	if !m.BodyPurged {
		i.Untrusted.Body, i.Trimmed = clip(m.BodyText, limit)
	}
	i.Untrusted.To, i.Untrusted.CC = m.To, m.CC
	return i
}

// filter bounds reads at the cursor's reference instant, so newly received mail
// cannot shift subsequent pages. Backfills/updates still require a fresh query.
func (s *Service) filter(r Range, mailbox, bucket string, at time.Time) (store.MessageFilter, error) {
	if err := s.validBucket(bucket); err != nil {
		return store.MessageFilter{}, err
	}
	since, until, err := r.parse(at)
	if err != nil {
		return store.MessageFilter{}, err
	}
	ceiling := at.Add(time.Millisecond) // store timestamps have millisecond precision
	if until == nil || until.After(ceiling) {
		until = &ceiling
	}
	f := store.MessageFilter{Mailbox: mailbox, Bucket: bucket, Since: since, Until: until}
	if bucket == unclassified {
		f.Bucket, f.UnclassifiedOnly = "", true
	}
	return f, nil
}
