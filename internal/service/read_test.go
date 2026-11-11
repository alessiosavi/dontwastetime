package service

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

var testNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

const testBox = "enabled@example.test"

type fixture struct {
	svc *Service
	db  *store.Store
	cfg *config.Config
	at  time.Time
	n   int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{db: db, at: testNow, cfg: &config.Config{
		Operators: []string{"founder"},
		Mailboxes: []config.MailboxConfig{{Address: testBox, Enabled: true},
			{Address: "disabled@example.test", Enabled: false}},
	}}
	for _, box := range f.cfg.Mailboxes {
		if err := db.UpsertMailbox(context.Background(), box); err != nil {
			t.Fatal(err)
		}
	}
	tax := &classify.Taxonomy{Order: []string{"clienti", "personal", "unsure"}, Labels: map[string]classify.Label{
		"clienti":  {Key: "clienti", Title: "Clienti"},
		"personal": {Key: "personal", Title: "Personale"},
		"unsure":   {Key: "unsure", Title: "Da verificare"},
	}}
	f.svc, err = New(db, f.cfg, tax, Options{Operator: "founder"}, func() time.Time { return f.at })
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) mail(t *testing.T, box, key, body, label string, at time.Time) int64 {
	t.Helper()
	f.n++
	m := graph.Message{ID: fmt.Sprintf("graph-%d", f.n), InternetMessageID: fmt.Sprintf("<mail-%d@external.test>", f.n),
		Mailbox: box, Folder: store.FolderInbox, From: "sentinel-sender@external.test", FromName: "sentinel-name",
		To: []string{"sentinel-to@external.test"}, CC: []string{"sentinel-cc@external.test"},
		Subject: "sentinel-subject", BodyText: body, BodyPreview: "sentinel-preview", ReceivedAt: at, HasAttachments: true}
	id, _, err := f.db.UpsertMessage(context.Background(), m, key, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpsertThreadFromMessage(context.Background(), id, at); err != nil {
		t.Fatal(err)
	}
	if label != "" {
		err = f.db.SaveClassification(context.Background(), store.Classification{MessageID: id,
			Label: label, Method: "llm", Confidence: .9, Rationale: "sentinel-rationale"}, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (f *fixture) obligation(t *testing.T, key, priority string, received, due time.Time, needs bool) int64 {
	t.Helper()
	id := f.mail(t, testBox, key, "sentinel-body", "clienti", received)
	err := f.db.SaveThreadState(context.Background(), store.ThreadRow{ThreadKey: key, Mailbox: testBox,
		LastInboundAt: &received, LastInboundMessageID: id, ReplyDueAt: &due,
		NeedsReply: needs, Priority: priority, Cassetto: "clienti"}, f.at)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSearch_RecentRelevantBeatsOldEquallyRelevant(t *testing.T) {
	f := setup(t)
	old := f.mail(t, testBox, "old", "contratto identico", "clienti", f.at.Add(-90*24*time.Hour))
	recent := f.mail(t, testBox, "recent", "contratto identico", "clienti", f.at.Add(-2*24*time.Hour))
	for _, order := range []string{"", "relevance", "recent"} {
		r, err := f.svc.Search(context.Background(), SearchReq{Query: "contratto", Order: order})
		if err != nil || len(r.Items) != 2 || r.Items[0].ID != recent || r.Items[1].ID != old {
			t.Fatalf("order=%q items=%+v err=%v", order, r.Items, err)
		}
	}
}

func TestSearch_InvalidQueryIsInvalid(t *testing.T) {
	f := setup(t)
	for _, req := range []SearchReq{{Query: " \n\t"}, {Query: "word", Order: "wrong"}} {
		if _, err := f.svc.Search(context.Background(), req); !errors.Is(err, ErrInvalid) {
			t.Fatalf("request=%+v error=%v, want ErrInvalid", req, err)
		}
	}
	for _, q := range []string{`"fattura" OR *`, "NEAR(a b)", "-x", `a"b`} {
		if _, err := f.svc.Search(context.Background(), SearchReq{Query: q}); err != nil {
			t.Errorf("literal query %q: %v", q, err)
		}
	}
}

func TestNeedsReply_ShowsDueAndOverdueStates(t *testing.T) {
	f := setup(t)
	f.obligation(t, "due", "P2", f.at.Add(-time.Hour), f.at.Add(23*time.Hour), false)
	f.obligation(t, "overdue", "P1", f.at.Add(-25*time.Hour), f.at.Add(-time.Hour), true)
	f.obligation(t, "p0", "P0", f.at.Add(-2*time.Hour), f.at.Add(22*time.Hour), true)
	// A read must recognize elapsed deadlines even before the daemon sweeps.
	f.obligation(t, "elapsed", "P2", f.at.Add(-26*time.Hour), f.at.Add(-2*time.Hour), false)
	r, err := f.svc.NeedsReply(context.Background(), NeedsReplyReq{})
	if err != nil || len(r.Items) != 4 {
		t.Fatalf("items=%+v err=%v", r.Items, err)
	}
	want := map[string]string{"due": "due", "overdue": "overdue", "p0": "overdue", "elapsed": "overdue"}
	for _, item := range r.Items {
		if item.State != want[item.Thread] || item.InboundID != item.ID || item.DueAt.IsZero() {
			t.Errorf("unexpected obligation %+v", item)
		}
	}
	filtered, err := f.svc.NeedsReply(context.Background(), NeedsReplyReq{State: "due"})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].Thread != "due" {
		t.Fatalf("due filter=%+v err=%v", filtered.Items, err)
	}
	row, err := f.db.Thread(context.Background(), "elapsed")
	if err != nil || row.NeedsReply {
		t.Fatalf("read mutated persisted state: %+v err=%v", row, err)
	}
}

func TestGetThread_BoundsBodiesAndPaginates(t *testing.T) {
	f := setup(t)
	var ids []int64
	for i := 0; i < 11; i++ {
		ids = append(ids, f.mail(t, testBox, "long", strings.Repeat("è", 18000), "clienti", f.at.Add(time.Duration(i-12)*time.Hour)))
	}
	f.mail(t, "disabled@example.test", "long", "hidden-body", "clienti", f.at.Add(-time.Hour))
	r, err := f.svc.GetThread(context.Background(), fmt.Sprintf("#%d", ids[0]))
	if err != nil || len(r.Messages) != 10 || r.Meta.NextCursor == "" {
		t.Fatalf("messages=%d cursor=%q err=%v", len(r.Messages), r.Meta.NextCursor, err)
	}
	chars := 0
	for i, m := range r.Messages {
		n := utf8.RuneCountInString(m.Untrusted.Body)
		chars += n
		if m.ID != ids[i] || n != 4000 || !m.Trimmed {
			t.Fatalf("message=%d id=%d body chars=%d trimmed=%v", i, m.ID, n, m.Trimmed)
		}
	}
	if chars > 40000 {
		t.Fatalf("body chars=%d", chars)
	}
	f.at = f.at.Add(time.Hour)
	next, err := f.svc.GetThreadPage(context.Background(), "long", Page{Cursor: r.Meta.NextCursor})
	if err != nil || len(next.Messages) != 1 || next.Messages[0].ID != ids[10] || next.Meta.NextCursor != "" || !next.Meta.AsOf.Equal(r.Meta.AsOf) {
		t.Fatalf("continuation=%+v err=%v", next, err)
	}
	// Three trimmed bodies total 12k, so only an explicit item limit pages them.
	small := setup(t)
	for i := 0; i < 3; i++ {
		small.mail(t, testBox, "three", strings.Repeat("x", 18000), "clienti", small.at.Add(time.Duration(i-4)*time.Hour))
	}
	p, err := small.svc.GetThreadPage(context.Background(), "three", Page{Limit: 2})
	if err != nil || len(p.Messages) != 2 || p.Meta.NextCursor == "" {
		t.Fatalf("explicit page limit: messages=%d err=%v", len(p.Messages), err)
	}
}

func TestGetMessage_OutOfScopeIsNotFound(t *testing.T) {
	f := setup(t)
	visible := f.mail(t, testBox, "shared", "visible-body", "personal", f.at.Add(-time.Hour))
	hidden := f.mail(t, "disabled@example.test", "shared", "hidden-body", "clienti", f.at)
	removed := f.mail(t, "removed@example.test", "removed", "hidden-body", "clienti", f.at)
	for _, id := range []int64{hidden, removed, 999999} {
		if _, err := f.svc.GetMessage(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("id=%d err=%v", id, err)
		}
		if _, err := f.svc.GetThread(context.Background(), fmt.Sprintf("#%d", id)); !errors.Is(err, ErrNotFound) {
			t.Errorf("thread through hidden id=%d err=%v", id, err)
		}
	}
	m, err := f.svc.GetMessage(context.Background(), visible)
	if err != nil || m.Bucket != "personal" {
		t.Fatalf("personal mail=%+v err=%v", m, err)
	}
}

func TestRange_ParsesDatesAndRelative(t *testing.T) {
	at := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		r            Range
		since, until time.Time
	}{
		{Range{Since: "2026-03-29", Until: "2026-03-30"}, time.Date(2026, 3, 28, 23, 0, 0, 0, time.UTC), time.Date(2026, 3, 29, 22, 0, 0, 0, time.UTC)},
		{Range{Since: "7d", Until: "2h"}, at.Add(-7 * 24 * time.Hour), at.Add(-2 * time.Hour)},
	} {
		since, until, err := tc.r.parse(at)
		if err != nil || since == nil || until == nil || !since.Equal(tc.since) || !until.Equal(tc.until) {
			t.Errorf("range=%+v got=%v/%v err=%v", tc.r, since, until, err)
		}
	}
	for _, r := range []Range{{Since: "yesterday"}, {Until: "2026-02-30"}, {Since: "-2d"}, {Since: "999999999999d"}, {Since: "2026-11-11", Until: "2026-01-01"}} {
		if _, _, err := r.parse(at); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid range=%+v err=%v", r, err)
		}
	}
}

func TestPage_ClampsLimitAndKeepsAsOf(t *testing.T) {
	f := setup(t)
	for i := 0; i < 24; i++ {
		f.mail(t, testBox, "page", "body", "clienti", f.at.Add(time.Duration(-i-1)*time.Hour))
	}
	r, err := f.svc.ListMessages(context.Background(), ListReq{})
	if err != nil || len(r.Items) != 20 || r.Meta.NextCursor == "" {
		t.Fatalf("default page=%d cursor=%q err=%v", len(r.Items), r.Meta.NextCursor, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(r.Meta.NextCursor)
	var cursor map[string]int64
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor["o"] != 20 || cursor["a"] != f.at.UnixMilli() || len(cursor) != 2 {
		t.Fatalf("cursor shape=%q err=%v", raw, err)
	}
	f.at = f.at.Add(2 * time.Hour)
	f.mail(t, testBox, "new", "new mail", "clienti", f.at)
	next, err := f.svc.ListMessages(context.Background(), ListReq{Page: Page{Cursor: r.Meta.NextCursor}})
	if err != nil || len(next.Items) != 4 || !next.Meta.AsOf.Equal(r.Meta.AsOf) || next.Meta.NextCursor != "" {
		t.Fatalf("next page=%+v err=%v", next, err)
	}
	p, err := (Page{Limit: 1000}).parse(f.at)
	if err != nil || p.limit != 100 {
		t.Fatalf("clamped page=%+v err=%v", p, err)
	}
	for _, p := range []Page{{Limit: -1}, {Cursor: "bad"}, {Cursor: base64.RawURLEncoding.EncodeToString([]byte(`{"o":-1,"a":1}`))},
		{Cursor: base64.RawURLEncoding.EncodeToString([]byte(`{}`))}, {Cursor: base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"o":0,"a":%d}`, f.at.UnixMilli()+1)))}} {
		if _, err := p.parse(f.at); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid page=%+v err=%v", p, err)
		}
	}
}

func TestResults_MailTextOnlyUnderUntrusted(t *testing.T) {
	f := setup(t)
	id := f.obligation(t, "privacy", "P0", f.at.Add(-time.Hour), f.at.Add(time.Hour), true)
	ctx := context.Background()
	contextResult, e1 := f.svc.Context(ctx)
	overview, e2 := f.svc.Overview(ctx, OverviewReq{})
	needs, e3 := f.svc.NeedsReply(ctx, NeedsReplyReq{})
	list, e4 := f.svc.ListMessages(ctx, ListReq{})
	search, e5 := f.svc.Search(ctx, SearchReq{Query: "sentinel"})
	thread, e6 := f.svc.GetThread(ctx, "privacy")
	message, e7 := f.svc.GetMessage(ctx, id)
	for _, err := range []error{e1, e2, e3, e4, e5, e6, e7} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, result := range []any{contextResult, overview, needs, list, search, thread, message} {
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		var walk func(any, bool)
		walk = func(v any, untrusted bool) {
			switch v := v.(type) {
			case map[string]any:
				for k, value := range v {
					walk(value, untrusted || k == "untrusted")
				}
			case []any:
				for _, value := range v {
					walk(value, untrusted)
				}
			case string:
				if strings.Contains(v, "sentinel-") && !untrusted {
					t.Errorf("%T exposes mail text outside untrusted: %q", result, v)
				}
			}
		}
		walk(doc, false)
		if !strings.Contains(string(raw), `"meta":{"as_of":`) {
			t.Errorf("%T missing meta/as_of", result)
		}
	}
	if message.Untrusted.Body != "sentinel-body" || !slices.Equal(message.Untrusted.To, []string{"sentinel-to@external.test"}) || message.Untrusted.Rationale != "sentinel-rationale" {
		t.Fatalf("detail omitted untrusted mail text: %+v", message.Untrusted)
	}
}

func TestContext_OperatorScopeAndFreshness(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	completed := f.at.Add(-time.Hour)
	for _, call := range []func() error{
		func() error { return f.db.SetSyncResult(ctx, testBox, false, completed) },
		func() error { return f.db.SetSyncResult(ctx, testBox, true, f.at) },
		func() error { return f.db.SetMailboxState(ctx, testBox, store.StateDegraded, "offline", f.at) },
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	f.mail(t, testBox, "unclassified", "body", "", f.at.Add(-time.Hour))
	f.mail(t, "disabled@example.test", "hidden", "body", "", f.at.Add(-time.Hour))
	f.at = time.Date(2026, 11, 11, 23, 30, 0, 0, time.UTC)
	r, err := f.svc.Context(ctx)
	if err != nil || r.Today != "2026-11-12" || r.Operator != "founder" || r.Meta.Unclassified != 1 || len(r.Mailboxes) != 1 {
		t.Fatalf("context=%+v err=%v", r, err)
	}
	fresh := r.Meta.Freshness[0]
	if fresh.LastSyncCompletedAt == nil || !fresh.LastSyncCompletedAt.Equal(completed) || !fresh.CatchingUp || fresh.State != "degraded" || fresh.Reason != "offline" {
		t.Fatalf("freshness=%+v", fresh)
	}
	if !slices.ContainsFunc(r.Buckets, func(b Bucket) bool { return b.Key == "personal" && b.Title == "Personale" }) {
		t.Fatalf("missing personal bucket: %+v", r.Buckets)
	}
	for _, name := range []string{"", "unknown", "cli"} {
		if _, err := New(f.db, f.cfg, f.svc.taxonomy, Options{Operator: name}, func() time.Time { return f.at }); !errors.Is(err, ErrUnknownOperator) {
			t.Errorf("operator=%q err=%v", name, err)
		}
	}
	cli, err := New(f.db, f.cfg, f.svc.taxonomy, Options{CLI: true, Operator: "ignored"}, func() time.Time { return f.at })
	if err != nil {
		t.Fatal(err)
	}
	cliContext, err := cli.Context(ctx)
	if err != nil || cliContext.Operator != "cli" || !reflect.DeepEqual(cliContext.Mailboxes, r.Mailboxes) {
		t.Fatalf("CLI context=%+v err=%v", cliContext, err)
	}
}

func TestOverview_CountsScopeRangeAndObligations(t *testing.T) {
	f := setup(t)
	f.mail(t, testBox, "personal", "body", "personal", f.at.Add(-time.Hour))
	f.mail(t, testBox, "pending", "body", "", f.at.Add(-time.Hour))
	f.mail(t, testBox, "unsure", "body", "unsure", f.at.Add(-time.Hour))
	f.mail(t, "disabled@example.test", "hidden", "body", "unsure", f.at.Add(-time.Hour))
	f.mail(t, testBox, "old", "body", "unsure", f.at.Add(-90*24*time.Hour))
	f.obligation(t, "p0", "P0", f.at.Add(-time.Hour), f.at.Add(time.Hour), true)
	r, err := f.svc.Overview(context.Background(), OverviewReq{Range: Range{Since: "7d"}})
	if err != nil || r.Unsure != 1 || r.Unclassified != 1 || len(r.P0) != 1 || len(r.Obligations) != 1 || r.Obligations[0].State != "overdue" {
		t.Fatalf("overview=%+v err=%v", r, err)
	}
	total := 0
	for _, c := range r.Counts {
		total += c.Count
		if c.Mailbox != testBox {
			t.Errorf("hidden mailbox in counts: %+v", c)
		}
	}
	if total != 4 {
		t.Errorf("total=%d, want 4", total)
	}
}

func TestListMessages_FiltersPurgingAndDetailBounds(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	long := f.mail(t, testBox, "long", strings.Repeat("界", 21000), "clienti", f.at.Add(-time.Hour))
	pending := f.mail(t, testBox, "pending", "body", "", f.at.Add(-2*time.Hour))
	old := f.mail(t, testBox, "purged", "old body", "personal", f.at.Add(-400*24*time.Hour))
	if _, err := f.db.PurgeBodies(ctx, f.at.Add(-365*24*time.Hour), f.at); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.ListMessages(ctx, ListReq{Bucket: "clienti", From: "SENTINEL-SENDER@EXTERNAL.TEST", Domain: "external.test", Direction: "in", Range: Range{Since: "7d"}})
	if err != nil || len(r.Items) != 1 || r.Items[0].ID != long || utf8.RuneCountInString(r.Items[0].Untrusted.Preview) != 300 || r.Items[0].Age != "1 ora" {
		t.Fatalf("filtered items=%+v err=%v", r.Items, err)
	}
	d, err := f.svc.GetMessage(ctx, long)
	if err != nil || utf8.RuneCountInString(d.Untrusted.Body) != 20000 || !d.Trimmed || d.Method != "llm" || d.Confidence != .9 {
		t.Fatalf("detail chars=%d trimmed=%v err=%v", utf8.RuneCountInString(d.Untrusted.Body), d.Trimmed, err)
	}
	p, err := f.svc.GetMessage(ctx, old)
	if err != nil || !p.BodyPurged || p.Untrusted.Body != "" || p.Trimmed {
		t.Fatalf("purged detail=%+v err=%v", p, err)
	}
	u, err := f.svc.GetMessage(ctx, pending)
	if err != nil || u.Bucket != "non classificato" || u.BucketTitle != "non classificato" {
		t.Fatalf("unclassified detail=%+v err=%v", u, err)
	}
	for _, req := range []ListReq{{Direction: "invalid"}, {Bucket: "invalid"}} {
		if _, err := f.svc.ListMessages(ctx, req); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid filter=%+v err=%v", req, err)
		}
	}
}

func TestNeedsReply_SuppressionAndCrossMailboxPrivacy(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, state := range []string{"handled", "snoozed", "muted", "expired"} {
		f.obligation(t, state, "P1", f.at.Add(-48*time.Hour), f.at.Add(-24*time.Hour), false)
		until := f.at.Add(time.Hour)
		if state == "expired" {
			until = f.at.Add(-time.Hour)
		}
		ack := state
		if state == "expired" {
			ack = "snoozed"
		}
		var muted any
		if state == "muted" {
			ack, muted = "", f.at.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		if _, err := f.db.DB().Exec(`UPDATE thread SET ack_kind = ?, snoozed_until = ?, muted_at = ? WHERE thread_key = ?`, ack, until.UTC().Format("2006-01-02T15:04:05.000Z"), muted, state); err != nil {
			t.Fatal(err)
		}
	}
	visible := f.obligation(t, "shared", "P1", f.at.Add(-48*time.Hour), f.at.Add(-24*time.Hour), true)
	hidden := f.mail(t, "disabled@example.test", "shared", "secret hidden body", "clienti", f.at.Add(-time.Hour))
	if _, err := f.db.DB().Exec(`UPDATE thread SET subject = 'hidden-subject', last_inbound_from = 'hidden-from', mailbox = 'disabled@example.test' WHERE thread_key = 'shared'`); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.NeedsReply(ctx, NeedsReplyReq{})
	if err != nil || len(r.Items) != 2 || !slices.ContainsFunc(r.Items, func(i ReplyItem) bool {
		return i.ID == visible && i.Mailbox == testBox && i.Untrusted.Subject == "sentinel-subject"
	}) {
		t.Fatalf("obligations=%+v err=%v", r.Items, err)
	}
	// A hidden last inbound must hide the obligation even if the thread has a visible copy.
	if _, err := f.db.DB().Exec(`UPDATE thread SET last_inbound_message_id = ? WHERE thread_key = 'shared'`, hidden); err != nil {
		t.Fatal(err)
	}
	r, err = f.svc.NeedsReply(ctx, NeedsReplyReq{})
	if err != nil || len(r.Items) != 1 || r.Items[0].Thread != "expired" || r.Items[0].State != "overdue" {
		t.Fatalf("hidden inbound obligations=%+v err=%v", r.Items, err)
	}
	thread, err := f.svc.GetThread(ctx, "handled")
	if err != nil || thread.ReplyState != threads.StateOf(store.ThreadRow{AckKind: "handled"}, f.at) {
		t.Fatalf("handled thread=%+v err=%v", thread, err)
	}
}

func TestSearch_UnclassifiedFilterBeforeCandidateCap(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	pending := f.mail(t, testBox, "pending", "contratto "+strings.Repeat("filler ", 100), "", f.at.Add(-time.Hour))
	// More strongly matching classified messages must not exhaust the search
	// cap before the requested unclassified bucket is applied.
	err := f.db.WithTx(ctx, func(tx *sql.Tx) error {
		for i := 0; i < 501; i++ {
			m := graph.Message{ID: fmt.Sprintf("classified-%d", i), InternetMessageID: fmt.Sprintf("<classified-%d@x>", i),
				Mailbox: testBox, Folder: store.FolderInbox, BodyText: "contratto", ReceivedAt: f.at.Add(-time.Hour)}
			id, _, err := f.db.UpsertMessageTx(ctx, tx, m, "classified", f.at)
			if err != nil {
				return err
			}
			if err := store.SaveClassificationTx(ctx, tx, store.Classification{MessageID: id, Label: "clienti", Method: "llm"}, f.at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.Search(ctx, SearchReq{Query: "contratto", Bucket: "non classificato"})
	if err != nil || len(r.Items) != 1 || r.Items[0].ID != pending {
		t.Fatalf("unclassified items=%d err=%v, want id %d", len(r.Items), err, pending)
	}
	l, err := f.svc.ListMessages(ctx, ListReq{Bucket: "non classificato"})
	if err != nil || len(l.Items) != 1 || l.Items[0].ID != pending {
		t.Fatalf("unclassified list=%+v err=%v", l.Items, err)
	}
}

func TestNeedsReply_UsesThreadBucketDuringClassificationBacklog(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.mail(t, testBox, "backlog", "classified message", "clienti", f.at.Add(-2*time.Hour))
	latest := f.mail(t, testBox, "backlog", "unclassified inbound", "", f.at.Add(-time.Hour))
	if _, err := threads.Recompute(ctx, f.db, "backlog", f.cfg, f.at); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []string{"", "clienti", unclassified} {
		r, err := f.svc.NeedsReply(ctx, NeedsReplyReq{Bucket: bucket})
		if err != nil {
			t.Fatal(err)
		}
		if bucket == unclassified {
			if len(r.Items) != 0 {
				t.Errorf("unclassified filter returned classified thread: %+v", r.Items)
			}
			continue
		}
		if len(r.Items) != 1 || r.Items[0].ID != latest || r.Items[0].Bucket != "clienti" || r.Items[0].BucketTitle != "Clienti" {
			t.Errorf("bucket=%q items=%+v, want latest inbound with thread bucket clienti/Clienti", bucket, r.Items)
		}
	}
	// The unclassified filter still matches an obligation whose thread bucket is empty.
	id := f.obligation(t, "empty-bucket", "P2", f.at.Add(-time.Hour), f.at.Add(time.Hour), false)
	if _, err := f.db.DB().Exec(`UPDATE thread SET cassetto='' WHERE thread_key='empty-bucket'`); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.NeedsReply(ctx, NeedsReplyReq{Bucket: unclassified})
	if err != nil || len(r.Items) != 1 || r.Items[0].ID != id || r.Items[0].Bucket != unclassified || r.Items[0].BucketTitle != unclassified {
		t.Errorf("empty thread bucket=%+v err=%v", r.Items, err)
	}
}

func TestSearch_RecentCapsAfterDateOrdering(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	newest := f.mail(t, testBox, "newest", "contratto "+strings.Repeat("filler ", 1000), "", f.at.Add(-time.Minute))
	err := f.db.WithTx(ctx, func(tx *sql.Tx) error {
		for i := 0; i < 599; i++ {
			m := graph.Message{ID: fmt.Sprintf("relevant-%d", i), InternetMessageID: fmt.Sprintf("<relevant-%d@x>", i),
				Mailbox: testBox, Folder: store.FolderInbox, BodyText: "contratto", ReceivedAt: f.at.Add(-time.Hour)}
			if _, _, err := f.db.UpsertMessageTx(ctx, tx, m, "older", f.at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.Search(ctx, SearchReq{Query: "contratto", Order: "recent", Page: Page{Limit: 1}})
	if err != nil || len(r.Items) != 1 || r.Items[0].ID != newest {
		t.Fatalf("recent search: items=%+v err=%v, want newest id %d despite weakest BM25", r.Items, err, newest)
	}
}

func TestReads_IncludeMailEarlierInSameSecond(t *testing.T) {
	f := setup(t)
	f.at = f.at.Add(800*time.Millisecond + 123456*time.Nanosecond)
	ctx := context.Background()
	id := f.obligation(t, "subsecond", "P2", f.at.Add(-300*time.Millisecond), f.at.Add(time.Hour), false)
	for _, name := range []string{"list_messages", "get_thread", "needs_reply", "search"} {
		t.Run(name, func(t *testing.T) {
			var items []MessageItem
			var err error
			switch name {
			case "list_messages":
				var r ListResult
				r, err = f.svc.ListMessages(ctx, ListReq{})
				items = r.Items
			case "get_thread":
				var r ThreadResult
				r, err = f.svc.GetThread(ctx, "subsecond")
				items = r.Messages
			case "needs_reply":
				var r NeedsReplyResult
				r, err = f.svc.NeedsReply(ctx, NeedsReplyReq{})
				for _, item := range r.Items {
					items = append(items, item.MessageItem)
				}
			case "search":
				var r ListResult
				r, err = f.svc.Search(ctx, SearchReq{Query: "sentinel"})
				items = r.Items
			}
			if err != nil || len(items) != 1 || items[0].ID != id {
				t.Errorf("same-second message excluded: items=%+v err=%v, want id %d", items, err, id)
			}
		})
	}
}

func TestPage_CursorPreservesMilliseconds(t *testing.T) {
	at := testNow.Add(800*time.Millisecond + 123456*time.Nanosecond)
	p, err := (Page{}).parse(at)
	if err != nil || !p.asOf.Equal(time.UnixMilli(at.UnixMilli())) {
		t.Errorf("first page as_of=%v err=%v, want millisecond precision", p.asOf, err)
	}
	encoded := p.next(1)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	var c cursor
	if err != nil || json.Unmarshal(raw, &c) != nil || c.AsOf != at.UnixMilli() {
		t.Errorf("cursor=%s err=%v, want millisecond timestamp %d", raw, err, at.UnixMilli())
	}
	resumed, err := (Page{Cursor: encoded}).parse(at.Add(time.Second))
	if err != nil || !resumed.asOf.Equal(time.UnixMilli(at.UnixMilli())) || resumed.offset != 1 {
		t.Errorf("resumed page=%+v err=%v", resumed, err)
	}
	future, _ := json.Marshal(cursor{Offset: 0, AsOf: at.UnixMilli() + 1})
	if _, err := (Page{Cursor: base64.RawURLEncoding.EncodeToString(future)}).parse(at); !errors.Is(err, ErrInvalid) {
		t.Errorf("future millisecond cursor: %v, want ErrInvalid", err)
	}
}
