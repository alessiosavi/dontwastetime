package service

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

func (s *Service) Context(ctx context.Context) (ContextResult, error) {
	at := s.now()
	m, err := s.meta(ctx, at)
	if err != nil {
		return ContextResult{}, err
	}
	r := ContextResult{Meta: m, Today: config.Day(at), Operator: s.operator,
		Mailboxes: m.Freshness, Buckets: []Bucket{}}
	for _, key := range s.taxonomy.Order {
		r.Buckets = append(r.Buckets, Bucket{Key: key, Title: s.taxonomy.Title(key)})
	}
	r.Buckets = append(r.Buckets, Bucket{Key: unclassified, Title: unclassified})
	return r, nil
}

// Overview's range applies to message counts; obligations describe the current
// open work regardless of its age. Its P0 list is bounded to 100; NextCursor can
// be passed to NeedsReply with the same mailbox and Priority="P0".
func (s *Service) Overview(ctx context.Context, req OverviewReq) (OverviewResult, error) {
	at := s.now()
	f, err := s.filter(req.Range, req.Mailbox, "", at)
	if err != nil {
		return OverviewResult{}, err
	}
	v := s.visibility
	if req.Mailbox != "" {
		v = store.Visibility{}
		for _, box := range s.visibility.Mailboxes {
			if box == req.Mailbox {
				v.Mailboxes = []string{box}
			}
		}
	}
	since := time.Time{}
	if f.Since != nil {
		since = *f.Since
	}
	counts, unsure, unclassifiedCount, err := s.store.VisibleCounts(ctx, v, since, *f.Until)
	if err != nil {
		return OverviewResult{}, storeError(err)
	}
	m, err := s.meta(ctx, at)
	if err != nil {
		return OverviewResult{}, err
	}
	r := OverviewResult{Meta: m, Counts: []BucketCount{}, Obligations: []ObligationCount{},
		P0: []ReplyItem{}, Unsure: unsure, Unclassified: unclassifiedCount}
	for _, c := range counts {
		bucket, title := s.bucket(c.Bucket)
		r.Counts = append(r.Counts, BucketCount{Mailbox: c.Mailbox, Bucket: bucket, BucketTitle: title, Count: c.N})
	}
	items, err := s.replyItems(ctx, NeedsReplyReq{Mailbox: req.Mailbox}, at)
	if err != nil {
		return OverviewResult{}, err
	}
	indices := map[string]int{}
	p0Count := 0
	for _, item := range items {
		key := item.Priority + ":" + item.State
		index, ok := indices[key]
		if !ok {
			index = len(r.Obligations)
			indices[key] = index
			r.Obligations = append(r.Obligations, ObligationCount{Priority: item.Priority, State: item.State})
		}
		r.Obligations[index].Count++
		if item.Priority == threads.PriorityP0 {
			p0Count++
			if len(r.P0) < maxLimit {
				r.P0 = append(r.P0, item)
			}
		}
	}
	if p0Count > maxLimit {
		r.Meta.NextCursor = (pageState{asOf: time.UnixMilli(at.UnixMilli()).UTC()}).next(maxLimit)
	}
	return r, nil
}

// replyState projects elapsed deadlines at the caller's reference instant.
// It leaves persisted thread state untouched; the daemon owns recomputation.
func replyState(row store.ThreadRow, at time.Time) string {
	if row.ReplyDueAt != nil {
		row.NeedsReply = row.NeedsReply || row.Priority == threads.PriorityP0 || !at.Before(*row.ReplyDueAt)
	}
	return threads.StateOf(row, at)
}

func (s *Service) replyItems(ctx context.Context, req NeedsReplyReq, at time.Time) ([]ReplyItem, error) {
	if err := s.validBucket(req.Bucket); err != nil {
		return nil, err
	}
	if req.State != "" && req.State != "due" && req.State != "overdue" {
		return nil, invalid("state deve essere due o overdue")
	}
	if req.Priority != "" && req.Priority != threads.PriorityP0 && req.Priority != threads.PriorityP1 &&
		req.Priority != threads.PriorityP2 && req.Priority != threads.PriorityPinned {
		return nil, invalid("priorità sconosciuta")
	}
	bucket := req.Bucket
	if bucket == unclassified {
		bucket = ""
	}
	rows, err := s.store.VisibleObligations(ctx, s.visibility, req.Mailbox, bucket, req.Priority, at)
	if err != nil {
		return nil, storeError(err)
	}
	items := []ReplyItem{}
	for _, row := range rows {
		state := replyState(row, at)
		if (state != "due" && state != "overdue") || (req.State != "" && req.State != state) ||
			(req.Bucket == unclassified && row.Cassetto != "") {
			continue
		}
		m, err := s.store.VisibleMessage(ctx, s.visibility, row.LastInboundMessageID)
		if errors.Is(err, store.ErrNotFound) {
			continue // a concurrent delete cannot disclose an unscoped fallback
		}
		if err != nil {
			return nil, storeError(err)
		}
		if m.ReceivedAt.After(at) {
			continue
		}
		item := s.item(m, at)
		item.Bucket, item.BucketTitle = s.bucket(row.Cassetto)
		items = append(items, ReplyItem{MessageItem: item, State: state,
			DueAt: *row.ReplyDueAt, Priority: row.Priority, InboundID: m.ID})
	}
	return items, nil
}

func (s *Service) NeedsReply(ctx context.Context, req NeedsReplyReq) (NeedsReplyResult, error) {
	p, err := req.Page.parse(s.now())
	if err != nil {
		return NeedsReplyResult{}, err
	}
	items, err := s.replyItems(ctx, req, p.asOf)
	if err != nil {
		return NeedsReplyResult{}, err
	}
	m, err := s.meta(ctx, p.asOf)
	if err != nil {
		return NeedsReplyResult{}, err
	}
	start := min(p.offset, len(items))
	end := min(start+p.limit, len(items))
	if end < len(items) {
		m.NextCursor = p.next(end)
	}
	return NeedsReplyResult{Meta: m, Items: items[start:end]}, nil
}

func (s *Service) ListMessages(ctx context.Context, req ListReq) (ListResult, error) {
	p, err := req.Page.parse(s.now())
	if err != nil {
		return ListResult{}, err
	}
	if req.Direction != "" && req.Direction != "in" && req.Direction != "out" {
		return ListResult{}, invalid("direction deve essere in o out")
	}
	f, err := s.filter(req.Range, req.Mailbox, req.Bucket, p.asOf)
	if err != nil {
		return ListResult{}, err
	}
	f.From, f.Domain, f.Direction, f.HasAttachments = req.From, req.Domain, req.Direction, req.HasAttachments
	rows, err := s.store.VisibleMessages(ctx, s.visibility, f, p.limit+1, p.offset)
	if err != nil {
		return ListResult{}, storeError(err)
	}
	m, err := s.meta(ctx, p.asOf)
	if err != nil {
		return ListResult{}, err
	}
	if len(rows) > p.limit {
		m.NextCursor = p.next(p.offset + p.limit)
		rows = rows[:p.limit]
	}
	r := ListResult{Meta: m, Items: []MessageItem{}}
	for _, row := range rows {
		r.Items = append(r.Items, s.item(row, p.asOf))
	}
	return r, nil
}

func (s *Service) Search(ctx context.Context, req SearchReq) (ListResult, error) {
	p, err := req.Page.parse(s.now())
	if err != nil {
		return ListResult{}, err
	}
	match, ok := store.FTSQuery(req.Query)
	if !ok {
		return ListResult{}, invalid("query vuota")
	}
	if req.Order != "" && req.Order != "relevance" && req.Order != "recent" {
		return ListResult{}, invalid("order deve essere recent o relevance")
	}
	f, err := s.filter(req.Range, req.Mailbox, req.Bucket, p.asOf)
	if err != nil {
		return ListResult{}, err
	}
	hits, err := s.store.SearchVisible(ctx, s.visibility, match, f, req.Order, searchCap)
	if err != nil {
		return ListResult{}, storeError(err)
	}
	type ranked struct {
		row   store.VisibleMessage
		score float64
	}
	rows := make([]ranked, 0, len(hits))
	for _, hit := range hits {
		row, err := s.store.VisibleMessage(ctx, s.visibility, hit.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return ListResult{}, storeError(err)
		}
		ageDays := max(0, p.asOf.Sub(row.ReceivedAt).Hours()/24)
		rows = append(rows, ranked{row: row, score: -hit.BM25 * math.Exp2(-ageDays/30)})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if req.Order != "recent" && a.score != b.score {
			return a.score > b.score
		}
		if !a.row.ReceivedAt.Equal(b.row.ReceivedAt) {
			return a.row.ReceivedAt.After(b.row.ReceivedAt)
		}
		return a.row.ID > b.row.ID
	})
	m, err := s.meta(ctx, p.asOf)
	if err != nil {
		return ListResult{}, err
	}
	start := min(p.offset, len(rows))
	end := min(start+p.limit, len(rows))
	if end < len(rows) {
		m.NextCursor = p.next(end)
	}
	r := ListResult{Meta: m, Items: []MessageItem{}}
	for _, row := range rows[start:end] {
		r.Items = append(r.Items, s.item(row.row, p.asOf))
	}
	return r, nil
}

// threadKey resolves a message reference through scope before looking up the
// shared thread. Triage methods can reuse it without trusting thread metadata.
func (s *Service) threadKey(ctx context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, "#") {
		id, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64)
		if err != nil || id <= 0 {
			return "", invalid("riferimento messaggio non valido")
		}
		m, err := s.store.VisibleMessage(ctx, s.visibility, id)
		if err != nil {
			return "", storeError(err)
		}
		return m.ThreadKey, nil
	}
	if ref == "" {
		return "", invalid("thread richiesto")
	}
	if _, err := s.store.VisibleThreadMessages(ctx, s.visibility, ref); err != nil {
		return "", storeError(err)
	}
	return ref, nil
}

// GetThread returns the first bounded page, preserving the simple read API.
func (s *Service) GetThread(ctx context.Context, ref string) (ThreadResult, error) {
	return s.GetThreadPage(ctx, ref, Page{})
}

// GetThreadPage accepts the continuation cursor returned by GetThread. The
// reference and filters must stay the same between pages.
func (s *Service) GetThreadPage(ctx context.Context, ref string, page Page) (ThreadResult, error) {
	p, err := page.parse(s.now())
	if err != nil {
		return ThreadResult{}, err
	}
	key, err := s.threadKey(ctx, ref)
	if err != nil {
		return ThreadResult{}, err
	}
	rows, err := s.store.VisibleThreadMessages(ctx, s.visibility, key)
	if err != nil {
		return ThreadResult{}, storeError(err)
	}
	visible := rows[:0]
	for _, row := range rows {
		if !row.ReceivedAt.After(p.asOf) {
			visible = append(visible, row)
		}
	}
	rows = visible
	if len(rows) == 0 {
		return ThreadResult{}, ErrNotFound
	}
	row, err := s.store.Thread(ctx, key)
	if errors.Is(storeError(err), ErrNotFound) {
		row, err = store.ThreadRow{ThreadKey: key}, nil // ingest may not have created state yet
	}
	if err != nil {
		return ThreadResult{}, storeError(err)
	}
	if row.LastInboundMessageID != 0 {
		inScope := false
		for _, m := range rows {
			inScope = inScope || m.ID == row.LastInboundMessageID
		}
		if !inScope {
			row = store.ThreadRow{ThreadKey: key}
		}
	}
	m, err := s.meta(ctx, p.asOf)
	if err != nil {
		return ThreadResult{}, err
	}
	r := ThreadResult{Meta: m, Thread: key, Messages: []MessageItem{},
		ReplyState: replyState(row, p.asOf), DueAt: row.ReplyDueAt, SnoozedUntil: row.SnoozedUntil}
	index, chars := min(p.offset, len(rows)), 0
	for index < len(rows) && len(r.Messages) < p.limit {
		item := s.detailItem(rows[index], p.asOf, threadChars)
		n := utf8.RuneCountInString(item.Untrusted.Body)
		if chars+n > pageChars {
			break
		}
		chars += n
		r.Messages = append(r.Messages, item)
		index++
	}
	if index < len(rows) {
		r.Meta.NextCursor = p.next(index)
	}
	return r, nil
}

func (s *Service) GetMessage(ctx context.Context, id int64) (MessageDetail, error) {
	at := s.now()
	row, err := s.store.VisibleMessage(ctx, s.visibility, id)
	if err != nil {
		return MessageDetail{}, storeError(err)
	}
	m, err := s.meta(ctx, at)
	if err != nil {
		return MessageDetail{}, err
	}
	item := s.detailItem(row, at, messageChars)
	item.Untrusted.Rationale = row.Rationale
	return MessageDetail{Meta: m, MessageItem: item, Method: row.Method, Confidence: row.Confidence}, nil
}
