package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

type RelabelResult struct {
	Old   string `json:"old"`
	New   string `json:"new"`
	State string `json:"state"`
}

type ThreadState struct {
	Thread       string     `json:"thread"`
	State        string     `json:"state"`
	DueAt        *time.Time `json:"due_at"`
	SnoozedUntil *time.Time `json:"snoozed_until"`
}

func (s *Service) Relabel(ctx context.Context, id int64, bucket string) (RelabelResult, error) {
	if bucket == "" || bucket == unclassified || !s.taxonomy.Has(bucket) {
		return RelabelResult{}, invalid("cassetto sconosciuto")
	}
	m, err := s.store.VisibleMessage(ctx, s.visibility, id)
	if err != nil {
		return RelabelResult{}, storeError(err)
	}
	at := s.now()
	var old string
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkTriageScopeTx(ctx, tx, m.ThreadKey); err != nil {
			return err
		}
		var err error
		old, err = s.store.AddCorrectionTx(ctx, tx, id, bucket, s.operator, at)
		return err
	})
	if err != nil {
		return RelabelResult{}, storeError(err)
	}
	// The correction has already enqueued a durable refresh. If immediate
	// recomputation fails, the daemon will still refresh the thread later.
	if _, err := threads.Recompute(ctx, s.store, m.ThreadKey, s.cfg, at); err != nil {
		return RelabelResult{}, storeError(err)
	}
	r, err := s.threadState(ctx, m.ThreadKey, at)
	if err != nil {
		return RelabelResult{}, err
	}
	return RelabelResult{Old: old, New: bucket, State: r.State}, nil
}

func (s *Service) MarkHandled(ctx context.Context, ref string, inboundID int64, note string) (ThreadState, error) {
	at := s.now()
	return s.triage(ctx, ref, at, func(tx *sql.Tx, key string) error {
		if err := s.visibleInbound(ctx, key, inboundID); err != nil {
			return err
		}
		return store.MarkHandledTx(ctx, tx, key, inboundID, s.operator, note, at)
	})
}

func (s *Service) Snooze(ctx context.Context, ref string, inboundID int64, until string) (ThreadState, error) {
	at := s.now()
	end, err := time.ParseInLocation(time.DateOnly, until, config.Rome)
	if err == nil {
		end = time.Date(end.Year(), end.Month(), end.Day(), 8, 0, 0, 0, config.Rome)
	} else if end, err = time.Parse(time.RFC3339, until); err != nil {
		return ThreadState{}, invalid("until deve essere YYYY-MM-DD o RFC3339")
	}
	if !end.After(at) || end.After(at.Add(90*24*time.Hour)) {
		return ThreadState{}, invalid("until deve essere nel futuro, entro 90 giorni")
	}
	return s.triage(ctx, ref, at, func(tx *sql.Tx, key string) error {
		if err := s.visibleInbound(ctx, key, inboundID); err != nil {
			return err
		}
		return store.SnoozeTx(ctx, tx, key, inboundID, end, s.operator, at)
	})
}

func (s *Service) Mute(ctx context.Context, ref string) (ThreadState, error) {
	at := s.now()
	return s.triage(ctx, ref, at, func(tx *sql.Tx, key string) error {
		return store.MuteThreadTx(ctx, tx, key, s.operator, at)
	})
}

func (s *Service) Reopen(ctx context.Context, ref string) (ThreadState, error) {
	at := s.now()
	return s.triage(ctx, ref, at, func(tx *sql.Tx, key string) error {
		return store.ReopenTx(ctx, tx, key, s.operator, at)
	})
}

func (s *Service) visibleInbound(ctx context.Context, key string, id int64) error {
	m, err := s.store.VisibleMessage(ctx, s.visibility, id)
	if err != nil {
		return storeError(err)
	}
	if m.ThreadKey != key {
		return ErrNotFound
	}
	return nil
}

func (s *Service) triage(ctx context.Context, ref string, at time.Time, change func(*sql.Tx, string) error) (ThreadState, error) {
	key, err := s.threadKey(ctx, ref)
	if err != nil {
		return ThreadState{}, err
	}
	if _, err := threads.Triage(ctx, s.store, s.cfg, key, at, func(tx *sql.Tx) error {
		if err := s.checkTriageScopeTx(ctx, tx, key); err != nil {
			return err
		}
		return change(tx, key)
	}); err != nil {
		return ThreadState{}, storeError(err)
	}
	return s.threadState(ctx, key, at)
}

// Check within the write transaction so a concurrent refresh cannot replace
// the last inbound between authorization and mutation. Ingest may not have
// created the thread row yet; in that case there is no derived state to hide.
func (s *Service) checkTriageScopeTx(ctx context.Context, tx *sql.Tx, key string) error {
	r, err := store.ThreadTx(ctx, tx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return storeError(err)
	}
	if r.LastInboundMessageID != 0 {
		m, err := store.VisibleMessageTx(ctx, tx, s.visibility, r.LastInboundMessageID)
		if err != nil {
			return storeError(err)
		}
		if m.ThreadKey != key {
			return ErrNotFound
		}
	}
	return nil
}

func (s *Service) threadState(ctx context.Context, key string, at time.Time) (ThreadState, error) {
	r, err := s.store.Thread(ctx, key)
	if err != nil {
		return ThreadState{}, storeError(err)
	}
	if r.LastInboundMessageID != 0 {
		if err := s.visibleInbound(ctx, key, r.LastInboundMessageID); err != nil {
			return ThreadState{}, err
		}
	}
	return ThreadState{Thread: key, State: threads.StateOf(r, at), DueAt: r.ReplyDueAt, SnoozedUntil: r.SnoozedUntil}, nil
}
