package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
)

var now0 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "dwt.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func msg(mailbox, imid string, received time.Time) graph.Message {
	return graph.Message{
		ID: "g-" + imid, InternetMessageID: imid, ConversationID: "conv-" + imid,
		Mailbox: mailbox, Folder: "inbox", Subject: "Oggetto " + imid,
		From: "mario@client.example", FromName: "Mario", BodyText: "corpo " + imid, BodyPreview: "corpo",
		To: []string{mailbox}, CC: []string{"cc@example.test"}, ReceivedAt: received, SentAt: received.Add(-time.Minute),
	}
}

func upsert(t *testing.T, s *Store, m graph.Message) (int64, bool) {
	t.Helper()
	id, inserted, err := s.UpsertMessage(context.Background(), m, "tk-"+m.InternetMessageID, now0)
	if err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	return id, inserted
}

func TestOpen_MigratesIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dwt.db")
	for range 2 {
		s, err := Open(p)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		var v int
		if err := s.DB().QueryRow(`SELECT max(version) FROM schema_version`).Scan(&v); err != nil || v != len(migrations) {
			t.Fatalf("schema_version = %d (err %v), want %d", v, err, len(migrations))
		}
		for _, tbl := range []string{"mailbox", "message", "classification", "thread", "queue", "corrections", "llm_usage", "digest", "action_log"} {
			var n int
			if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil || n != 1 {
				t.Errorf("table %s missing", tbl)
			}
		}
		s.Close()
	}
}

func TestOpen_Pragmas(t *testing.T) {
	s := openTest(t)
	var mode string
	var fk, busy int
	_ = s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	_ = s.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	_ = s.DB().QueryRow(`PRAGMA busy_timeout`).Scan(&busy)
	if mode != "wal" || fk != 1 || busy != 5000 {
		t.Fatalf("journal_mode=%s foreign_keys=%d busy_timeout=%d", mode, fk, busy)
	}
}

func TestUpsertMessage_Dedupes(t *testing.T) {
	s := openTest(t)
	m := msg("marketing@example.test", "<a@x>", now0)
	id1, ins1 := upsert(t, s, m)
	id2, ins2 := upsert(t, s, m)
	if !ins1 || ins2 || id1 != id2 {
		t.Fatalf("first insert=%v second insert=%v ids %d/%d", ins1, ins2, id1, id2)
	}
	// Same internet id in another mailbox is a different row.
	if _, ins := upsert(t, s, msg("tech@example.test", "<a@x>", now0)); !ins {
		t.Fatal("same internetMessageId in a different mailbox must insert")
	}
	var n int
	_ = s.DB().QueryRow(`SELECT count(*) FROM message`).Scan(&n)
	if n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
}

func TestUpsertMessage_UpdatesChangedFields(t *testing.T) {
	s := openTest(t)
	m := msg("marketing@example.test", "<a@x>", now0)
	id, _ := upsert(t, s, m)
	m.Subject, m.ID, m.HasAttachments = "Oggetto corretto", "g-moved", true
	if _, ins := upsert(t, s, m); ins {
		t.Fatal("update reported as insert")
	}
	row, err := s.Message(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Subject != "Oggetto corretto" || row.GraphID != "g-moved" || !row.HasAttachments {
		t.Fatalf("fields not updated: %+v", row)
	}
	if row.Direction != "in" || row.ThreadKey != "tk-<a@x>" || row.From != "mario@client.example" || len(row.CC) != 1 {
		t.Fatalf("stored fields: %+v", row)
	}
	if !row.ReceivedAt.Equal(now0) || !row.InsertedAt.Equal(now0) {
		t.Fatalf("times: received %v inserted %v", row.ReceivedAt, row.InsertedAt)
	}
}

func TestCursor_Roundtrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.UpsertMailbox(ctx, config.MailboxConfig{Address: "m@example.test", Enabled: true, PollSeconds: 180}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Cursor(ctx, "m@example.test", "inbox"); err != nil || !c.IsZero() {
		t.Fatalf("fresh cursor = %v, %v", c, err)
	}
	want := time.Date(2026, 11, 9, 10, 11, 12, 345e6, time.UTC)
	if err := s.SetCursor(ctx, "m@example.test", "sentitems", want); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Cursor(ctx, "m@example.test", "sentitems"); !c.Equal(want) {
		t.Fatalf("cursor = %v, want %v", c, want)
	}
	if c, _ := s.Cursor(ctx, "m@example.test", "inbox"); !c.IsZero() {
		t.Fatalf("folders share a cursor: %v", c)
	}
	if err := s.SetCursor(ctx, "m@example.test", "drafts", want); err == nil {
		t.Fatal("unknown folder accepted")
	}
	if err := s.SetCursor(ctx, "nobody@example.test", "inbox", want); err == nil {
		t.Fatal("unknown mailbox accepted")
	}
}

func TestUpdateMessageFlags(t *testing.T) {
	s := openTest(t)
	id, _ := upsert(t, s, msg("m@example.test", "<f@x>", now0))
	if err := s.UpdateMessageFlags(context.Background(), id, true, false, true); err != nil {
		t.Fatal(err)
	}
	row, _ := s.Message(context.Background(), id)
	if !row.IsAuto || row.IsBulk || !row.IsList {
		t.Fatalf("flags: auto=%v bulk=%v list=%v", row.IsAuto, row.IsBulk, row.IsList)
	}
}

func TestMessagesToClassify_ExcludesClassified(t *testing.T) {
	s := openTest(t)
	newer, _ := upsert(t, s, msg("m@example.test", "<n@x>", now0))
	older, _ := upsert(t, s, msg("m@example.test", "<o@x>", now0.Add(-time.Hour)))
	done, _ := upsert(t, s, msg("m@example.test", "<d@x>", now0.Add(-2*time.Hour)))
	if _, err := s.DB().Exec(`UPDATE message SET classified_at = ? WHERE id = ?`, ts(now0), done); err != nil {
		t.Fatal(err)
	}
	rows, err := s.MessagesToClassify(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != older || rows[1].ID != newer {
		t.Fatalf("want [older newer], got %+v", rows)
	}
}

func TestMailbox_UpsertPreservesRuntime(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mb := config.MailboxConfig{Address: "legal@example.test", Enabled: true, Digest: true, PollSeconds: 180}
	if err := s.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}
	cur := now0.Add(-time.Hour)
	_ = s.SetCursor(ctx, mb.Address, "inbox", cur)
	if err := s.SetMailboxState(ctx, mb.Address, StateDegraded, "403: access policy?", now0); err != nil {
		t.Fatal(err)
	}
	mb.Digest, mb.PollSeconds = false, 300
	if err := s.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListMailboxes(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListMailboxes: %v %v", rows, err)
	}
	r := rows[0]
	if r.State != StateDegraded || r.Reason != "403: access policy?" || !r.CursorInbox.Equal(cur) {
		t.Fatalf("runtime lost: %+v", r)
	}
	if r.Digest || r.PollSeconds != 300 || !r.Enabled {
		t.Fatalf("config columns not refreshed: %+v", r)
	}
	if err := s.SetMailboxState(ctx, mb.Address, StateOK, "", now0); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListMailboxes(ctx)
	if rows[0].LastOKAt == nil || !rows[0].LastOKAt.Equal(now0) || rows[0].Reason != "" {
		t.Fatalf("ok state: %+v", rows[0])
	}
}

func TestUsageAdd_Today(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.UsageAdd(ctx, "2026-11-10", 100, 20)
	_ = s.UsageAdd(ctx, "2026-11-10", 50, 5)
	_ = s.UsageAdd(ctx, "2026-11-09", 999, 999)
	in, out, err := s.UsageToday(ctx, "2026-11-10")
	if err != nil || in != 150 || out != 25 {
		t.Fatalf("usage = %d/%d (%v), want 150/25", in, out, err)
	}
	if in, out, _ := s.UsageToday(ctx, "2026-11-11"); in != 0 || out != 0 {
		t.Fatalf("empty day = %d/%d", in, out)
	}
}

func TestRecordDigest_DryRunNullSentAt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.RecordDigest(ctx, "2026-11-10", "/tmp/d.md", []string{"a@example.test"}, `{"ok":true}`, nil); err != nil {
		t.Fatal(err)
	}
	var sent *string
	var rcpt string
	_ = s.DB().QueryRow(`SELECT sent_at, recipients FROM digest WHERE date='2026-11-10'`).Scan(&sent, &rcpt)
	if sent != nil || rcpt == "" {
		t.Fatalf("dry run: sent_at=%v recipients=%q", sent, rcpt)
	}
	at := now0
	if err := s.RecordDigest(ctx, "2026-11-10", "/tmp/d.md", []string{"a@example.test"}, `{}`, &at); err != nil {
		t.Fatal(err)
	}
	_ = s.DB().QueryRow(`SELECT sent_at FROM digest WHERE date='2026-11-10'`).Scan(&sent)
	if sent == nil {
		t.Fatal("live send after dry run must set sent_at")
	}
}

func TestPurgeBodies_KeepsMetadata(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	old, _ := upsert(t, s, msg("m@example.test", "<old@x>", now0.AddDate(-1, 0, -2)))
	fresh, _ := upsert(t, s, msg("m@example.test", "<new@x>", now0))
	n, err := s.PurgeBodies(ctx, now0.AddDate(0, 0, -365), now0)
	if err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want 1", n, err)
	}
	o, _ := s.Message(ctx, old)
	if o.BodyText != "" || o.BodyPreview != "" || o.BodyPurgedAt == nil || o.Subject != "Oggetto <old@x>" || o.ReceivedAt.IsZero() {
		t.Fatalf("old row after purge: %+v", o)
	}
	if f, _ := s.Message(ctx, fresh); f.BodyText == "" {
		t.Fatal("fresh body purged")
	}
	var logged int
	_ = s.DB().QueryRow(`SELECT count(*) FROM action_log WHERE action='purge_bodies'`).Scan(&logged)
	if logged != 1 {
		t.Fatalf("purge not logged: %d", logged)
	}
	// Re-fetching the purged message (overlap window) must not resurrect the body.
	upsert(t, s, msg("m@example.test", "<old@x>", now0.AddDate(-1, 0, -2)))
	if o, _ := s.Message(ctx, old); o.BodyText != "" {
		t.Fatal("upsert resurrected a purged body")
	}
}

func TestLogAction(t *testing.T) {
	s := openTest(t)
	if err := s.LogAction(context.Background(), "cli", "correct", "message:7", map[string]any{"new": "clienti"}, now0); err != nil {
		t.Fatal(err)
	}
	var details string
	_ = s.DB().QueryRow(`SELECT details_json FROM action_log WHERE target='message:7'`).Scan(&details)
	if details != `{"new":"clienti"}` {
		t.Fatalf("details = %q", details)
	}
}

func TestRecordDigest_DryRunKeepsSentAt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := now0
	if ok, err := s.ClaimDigestSend(ctx, "2026-11-10", "/d.md", []string{"a@example.test"}, `{}`, at); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, _ := s.ClaimDigestSend(ctx, "2026-11-10", "/d.md", []string{"a@example.test"}, `{}`, at); ok {
		t.Fatal("second claim for the same day must lose")
	}
	if err := s.RecordDigest(ctx, "2026-11-10", "/d.md", []string{"a@example.test"}, `{}`, nil); err != nil {
		t.Fatal(err)
	}
	if sent, _ := s.DigestSent(ctx, "2026-11-10"); !sent {
		t.Fatal("a dry run erased the delivery marker")
	}
	if err := s.UnclaimDigestSend(ctx, "2026-11-10"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ClaimDigestSend(ctx, "2026-11-10", "/d.md", nil, `{}`, at); !ok {
		t.Fatal("unclaimed (failed) send must be claimable again")
	}
}

func TestClearClassifyLeases(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	upsert(t, s, msg("m@example.test", "<l@x>", now0))
	if rows, _ := s.ClaimUnclassified(ctx, 10, time.Hour, now0); len(rows) != 1 {
		t.Fatal("no lease taken")
	}
	if n, err := s.ClearClassifyLeases(ctx); err != nil || n != 1 {
		t.Fatalf("cleared %d (%v)", n, err)
	}
	if rows, _ := s.ClaimUnclassified(ctx, 10, time.Hour, now0); len(rows) != 1 {
		t.Fatal("message still leased after clearing")
	}
}
