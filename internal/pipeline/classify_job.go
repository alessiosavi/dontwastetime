package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/alessiosavi/GoGPUtils/sliceutil"
	"github.com/alessiosavi/GoGPUtils/stringutil"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

const (
	// ClassifyBatch is how many messages one classify job leases (and the
	// LLM batch size).
	ClassifyBatch = 10
	// ClassifyLease bounds how long a crashed job can hold its batch.
	ClassifyLease = 10 * time.Minute
	// itemPreviewLen bounds the body excerpt sent to the LLM.
	itemPreviewLen = 1000
	fewShot        = 20
)

// ClassifyHandler is the "classify" consumer. Each job leases a batch of
// unclassified messages; inbound ones get their headers (and attachment
// metadata) fetched, flags persisted and the rules applied; the rest go to
// the LLM in one call. Each classified message enqueues a thread_refresh.
// On any failure the batch's leases are released so the retry or a sibling
// job can take them; nothing is lost and nothing is classified twice.
func ClassifyHandler(s *store.Store, c *graph.Client, l *classify.LLM, t *classify.Taxonomy, cfg *config.Config, now func() time.Time) Handler {
	return func(ctx context.Context, job *store.Job) error {
		// No shortcut on job.Ref: a retry must re-claim the batch, because a
		// sibling's own job may have completed while this job held its lease.
		rows, err := s.ClaimUnclassified(ctx, ClassifyBatch, ClassifyLease, now())
		if err != nil || len(rows) == 0 {
			return err
		}
		done := map[int64]bool{}
		release := func() {
			var open []int64
			for _, r := range rows {
				if !done[r.ID] {
					open = append(open, r.ID)
				}
			}
			_ = s.ReleaseLease(context.WithoutCancel(ctx), open)
		}

		var ruled []store.Classification
		var toLLM []store.MessageRow
		for _, r := range rows {
			res, ok, err := applyRules(ctx, s, c, t, r)
			if err != nil {
				release()
				return err
			}
			if ok {
				ruled = append(ruled, toClassification(r.ID, res, t, 0))
				continue
			}
			toLLM = append(toLLM, r)
		}
		if err := saveClassified(ctx, s, ruled, now()); err != nil {
			release()
			return err
		}
		for _, cl := range ruled {
			done[cl.MessageID] = true
		}

		if len(toLLM) == 0 {
			return nil
		}
		examples, err := s.RecentCorrections(ctx, fewShot)
		if err != nil {
			release()
			return err
		}
		for _, chunk := range sliceutil.Chunk(toLLM, ClassifyBatch) {
			items := make([]classify.Item, len(chunk))
			for i, r := range chunk {
				items[i] = classify.Item{ID: r.ID, Mailbox: r.Mailbox, From: r.From, Subject: r.Subject,
					Preview: stringutil.Truncate(r.BodyText, itemPreviewLen, "…")}
			}
			results, usage, err := l.Classify(ctx, items, t, examples)
			if err != nil {
				release()
				if errors.Is(err, store.ErrBudgetReached) {
					return err
				}
				return fmt.Errorf("classify: llm: %w", err)
			}
			cost := (usage.In + usage.Out) / len(chunk)
			out := make([]store.Classification, len(results))
			for i, res := range results {
				out[i] = toClassification(res.ID, res, t, cost)
				out[i].Model = cfg.LLM.Model
			}
			if err := saveClassified(ctx, s, out, now()); err != nil {
				release()
				return err
			}
			for _, cl := range out {
				done[cl.MessageID] = true
			}
		}
		return nil
	}
}

// applyRules fetches headers/attachments for inbound mail, persists the
// flags, and runs the rules. Outbound mail skips straight to the LLM.
func applyRules(ctx context.Context, s *store.Store, c *graph.Client, t *classify.Taxonomy, r store.MessageRow) (classify.Result, bool, error) {
	if r.Direction != "in" {
		return classify.Result{}, false, nil
	}
	headers, err := c.MessageHeaders(ctx, r.Mailbox, r.GraphID)
	if err = degrade(ctx, r.ID, "headers", err); err != nil {
		return classify.Result{}, false, err
	}
	var atts []graph.Attachment
	if r.HasAttachments {
		atts, err = c.ListAttachments(ctx, r.Mailbox, r.GraphID)
		if err = degrade(ctx, r.ID, "attachments", err); err != nil {
			return classify.Result{}, false, err
		}
	}
	m := r.Graph()
	m.IsAuto, m.IsBulk, m.IsList = classify.Flags(headers)
	if err := s.UpdateMessageFlags(ctx, r.ID, m.IsAuto, m.IsBulk, m.IsList); err != nil {
		return classify.Result{}, false, err
	}
	res, ok := classify.Rules(m, headers, atts, t)
	return res, ok, nil
}

// degrade decides what a failed header/attachment fetch means. Throttling
// stops the batch (the pool waits for Retry-After); shutdown stops it; any
// other failure (403 after access scoping, 404 for a moved message, a 5xx on
// one item) classifies this message without that metadata, so one bad
// message never blocks the others.
func degrade(ctx context.Context, id int64, what string, err error) error {
	var te *graph.ErrThrottled
	switch {
	case err == nil:
		return nil
	case errors.As(err, &te), ctx.Err() != nil:
		return fmt.Errorf("classify: %s of message %d: %w", what, id, err)
	}
	slog.Default().Warn("classifying without "+what, "message", id, "err", err)
	return nil
}

func toClassification(id int64, r classify.Result, t *classify.Taxonomy, cost int) store.Classification {
	return store.Classification{MessageID: id, Label: r.Label, Entity: r.Entity, Confidence: r.Confidence,
		Urgent: r.Urgent, Negative: r.Negative, Method: r.Method, Rationale: r.Rationale, CostTokens: cost,
		TaxonomyVersion: t.Version}
}

// saveClassified writes the classifications and their thread_refresh jobs
// in one transaction.
func saveClassified(ctx context.Context, s *store.Store, cls []store.Classification, now time.Time) error {
	if len(cls) == 0 {
		return nil
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, c := range cls {
			if err := store.SaveClassificationTx(ctx, tx, c, now); err != nil {
				return err
			}
			if err := s.EnqueueTx(ctx, tx, KindThreadRefresh, c.MessageID, now); err != nil {
				return err
			}
		}
		return nil
	})
}
