package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

const mbA, mbB = "a@example.test", "b@example.test"

func testConfig(boxes ...string) *config.Config {
	cfg := &config.Config{PollSeconds: 180, PollOverlapMinutes: 30, InitialDays: 90, Workers: 1}
	for _, b := range boxes {
		cfg.Mailboxes = append(cfg.Mailboxes, config.MailboxConfig{Address: b, Enabled: true, Digest: true, PollSeconds: 180})
	}
	return cfg
}

// synth builds n inbound messages for mailbox, one hour apart, ending 1h before now0.
func synth(mailbox string, n int) []graph.Message {
	out := make([]graph.Message, n)
	for i := range n {
		at := now0.Add(-time.Duration(n-i) * time.Hour)
		out[i] = graph.Message{
			ID: fmt.Sprintf("g-%s-%d", mailbox, i), InternetMessageID: fmt.Sprintf("<%d@%s>", i, mailbox),
			ConversationID: fmt.Sprintf("conv-%d", i), Mailbox: mailbox, Folder: "inbox",
			Subject: fmt.Sprintf("Oggetto %d", i), From: "x@client.example", To: []string{mailbox},
			BodyText: "corpo", ReceivedAt: at, SentAt: at,
		}
	}
	return out
}

func setup(t *testing.T, boxes ...string) (*store.Store, *graphtest.MailFake, *graph.Client, *config.Config) {
	t.Helper()
	s := openStore(t)
	f, _, c := graphtest.NewMail(t)
	cfg := testConfig(boxes...)
	for _, mb := range cfg.Mailboxes {
		if err := s.UpsertMailbox(context.Background(), mb); err != nil {
			t.Fatal(err)
		}
	}
	return s, f, c, cfg
}

func count(t *testing.T, s *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSyncOnce_DedupesAcrossRuns(t *testing.T) {
	s, f, c, cfg := setup(t, mbA)
	f.Add(synth(mbA, 5)...)
	wake := make(chan struct{}, 1)
	p := NewPoller(c, s, cfg.Mailboxes[0], cfg, wake)
	fetched, inserted, err := p.SyncOnce(context.Background(), now0)
	if err != nil || fetched != 5 || inserted != 5 {
		t.Fatalf("first run fetched=%d inserted=%d err=%v", fetched, inserted, err)
	}
	select {
	case <-wake:
	default:
		t.Error("no wake signal after inserts")
	}
	// Three minutes later the overlap window re-reads recent mail: nothing new.
	_, inserted, err = p.SyncOnce(context.Background(), now0.Add(3*time.Minute))
	if err != nil || inserted != 0 {
		t.Fatalf("second run inserted=%d err=%v", inserted, err)
	}
	if n := count(t, s, `SELECT count(*) FROM queue WHERE kind='classify'`); n != 5 {
		t.Fatalf("classify jobs = %d, want 5 (one per new message)", n)
	}
}

func TestSyncOnce_CrashOnNthUpsertKeepsNothingLost(t *testing.T) {
	s, f, c, cfg := setup(t, mbA)
	f.Add(synth(mbA, 6)...)
	// Inject a failure before the 4th row's commit (rows 1-3 commit).
	if _, err := s.DB().Exec(`CREATE TRIGGER inject_crash BEFORE INSERT ON message
		WHEN (SELECT count(*) FROM message) >= 3 BEGIN SELECT RAISE(ABORT, 'injected crash'); END`); err != nil {
		t.Fatal(err)
	}
	p := NewPoller(c, s, cfg.Mailboxes[0], cfg, nil)
	if _, _, err := p.SyncOnce(context.Background(), now0); err == nil {
		t.Fatal("injected failure not surfaced")
	}
	if cur, _ := s.Cursor(context.Background(), mbA, "inbox"); !cur.IsZero() {
		t.Fatalf("cursor advanced past a failed folder: %v", cur)
	}
	if n := count(t, s, `SELECT count(*) FROM message`); n != 3 {
		t.Fatalf("committed rows = %d, want 3", n)
	}
	if _, err := s.DB().Exec(`DROP TRIGGER inject_crash`); err != nil {
		t.Fatal(err)
	}
	_, inserted, err := p.SyncOnce(context.Background(), now0.Add(time.Minute))
	if err != nil || inserted != 3 {
		t.Fatalf("re-run inserted=%d err=%v, want exactly the remaining 3", inserted, err)
	}
	if m, q := count(t, s, `SELECT count(*) FROM message`), count(t, s, `SELECT count(*) FROM queue`); m != 6 || q != 6 {
		t.Fatalf("messages=%d jobs=%d, want 6/6", m, q)
	}
}

func TestSyncOnce_CursorAdvancesToMaxReceived(t *testing.T) {
	s, f, c, cfg := setup(t, mbA)
	msgs := synth(mbA, 3)
	sent := msgs[1]
	sent.Folder, sent.InternetMessageID, sent.ReceivedAt = "sentitems", "<sent@a>", now0.Add(-10*time.Minute)
	f.Add(msgs...)
	f.Add(sent)
	p := NewPoller(c, s, cfg.Mailboxes[0], cfg, nil)
	if _, _, err := p.SyncOnce(context.Background(), now0); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Cursor(context.Background(), mbA, "inbox"); !cur.Equal(msgs[2].ReceivedAt) {
		t.Errorf("inbox cursor = %v, want %v", cur, msgs[2].ReceivedAt)
	}
	if cur, _ := s.Cursor(context.Background(), mbA, "sentitems"); !cur.Equal(sent.ReceivedAt) {
		t.Errorf("sentitems cursor = %v, want %v", cur, sent.ReceivedAt)
	}
	if n := count(t, s, `SELECT count(*) FROM message WHERE direction='out'`); n != 1 {
		t.Errorf("outbound rows = %d", n)
	}
}

func TestSyncOnce_TruncatedReadAdvancesWithoutLoss(t *testing.T) {
	s, f, c, cfg := setup(t, mbA)
	f.Add(synth(mbA, 7)...)
	p := NewPoller(c, s, cfg.Mailboxes[0], cfg, nil)
	p.Top, p.MaxPages = 2, 2 // 4 messages per run
	_, inserted, err := p.SyncOnce(context.Background(), now0)
	if err != nil || inserted != 4 {
		t.Fatalf("first capped run inserted=%d err=%v", inserted, err)
	}
	if n := count(t, s, `SELECT count(*) FROM action_log WHERE action='sync_truncated'`); n != 1 {
		t.Fatalf("truncation not recorded: %d", n)
	}
	for range 3 {
		if _, _, err := p.SyncOnce(context.Background(), now0); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM message`); n != 7 {
		t.Fatalf("after catch-up runs: %d messages, want 7", n)
	}
}

func assertSyncFreshness(t *testing.T, s *store.Store, completedAt *time.Time, truncated bool) {
	t.Helper()
	var completed sql.NullString
	var gotTruncated bool
	if err := s.DB().QueryRow(`SELECT last_sync_completed_at, sync_truncated FROM mailbox WHERE address = ?`, mbA).
		Scan(&completed, &gotTruncated); err != nil {
		t.Fatalf("read sync freshness: %v", err)
	}
	if gotTruncated != truncated {
		t.Errorf("sync_truncated = %v, want %v", gotTruncated, truncated)
	}
	if completedAt == nil {
		if completed.Valid {
			t.Errorf("last_sync_completed_at = %q, want NULL", completed.String)
		}
		return
	}
	got, err := time.Parse(time.RFC3339Nano, completed.String)
	if err != nil || !got.Equal(*completedAt) {
		t.Errorf("last_sync_completed_at = %q (%v), want %v", completed.String, err, *completedAt)
	}
}

func TestSyncOnce_TruncatedPassLeavesCompletedAtUnset(t *testing.T) {
	for _, folder := range []string{store.FolderInbox, store.FolderSentItems} {
		t.Run(folder, func(t *testing.T) {
			s, f, c, cfg := setup(t, mbA)
			msgs := synth(mbA, 7)
			for i := range msgs {
				msgs[i].Folder = folder
			}
			f.Add(msgs...)
			p := NewPoller(c, s, cfg.Mailboxes[0], cfg, nil)
			p.Top, p.MaxPages = 2, 2
			fetched, inserted, err := p.SyncOnce(context.Background(), now0)
			if err != nil || fetched != 4 || inserted != 4 {
				t.Fatalf("capped run fetched=%d inserted=%d err=%v", fetched, inserted, err)
			}
			assertSyncFreshness(t, s, nil, true)
		})
	}
}

func TestSyncOnce_CleanPassStampsCompletedAt(t *testing.T) {
	for _, n := range []int{0, 4} {
		t.Run(fmt.Sprintf("messages_%d", n), func(t *testing.T) {
			s, f, c, cfg := setup(t, mbA)
			if err := s.SetSyncResult(context.Background(), mbA, true, now0.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			f.Add(synth(mbA, n)...)
			p := NewPoller(c, s, cfg.Mailboxes[0], cfg, nil)
			p.Top, p.MaxPages = 2, 2
			if _, _, err := p.SyncOnce(context.Background(), now0); err != nil {
				t.Fatal(err)
			}
			assertSyncFreshness(t, s, &now0, false)
		})
	}
}

func TestSyncOnce_FailedPassPreservesFreshness(t *testing.T) {
	for _, state := range []string{"fresh", "completed", "truncated"} {
		t.Run(state, func(t *testing.T) {
			s, f, c, cfg := setup(t, mbA)
			ctx := context.Background()
			previous := now0.Add(-time.Hour)
			var completedAt *time.Time
			if state != "fresh" {
				if err := s.SetSyncResult(ctx, mbA, false, previous); err != nil {
					t.Fatal(err)
				}
				completedAt = &previous
			}
			if state == "truncated" {
				if err := s.SetSyncResult(ctx, mbA, true, previous.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			inbox := synth(mbA, 1)[0]
			sent := inbox
			sent.Folder, sent.InternetMessageID = store.FolderSentItems, "<sent@a>"
			f.Add(inbox, sent)
			if _, err := s.DB().Exec(`CREATE TRIGGER inject_sent_failure BEFORE INSERT ON message
				WHEN NEW.folder = 'sentitems' BEGIN SELECT RAISE(ABORT, 'injected sent failure'); END`); err != nil {
				t.Fatal(err)
			}
			p := NewPoller(c, s, cfg.Mailboxes[0], cfg, nil)
			_, inserted, err := p.SyncOnce(ctx, now0)
			if err == nil || !strings.Contains(err.Error(), "injected sent failure") || inserted != 1 {
				t.Fatalf("failed second folder inserted=%d err=%v", inserted, err)
			}
			assertSyncFreshness(t, s, completedAt, state == "truncated")
			rows, err := s.ListMailboxes(ctx)
			if err != nil || len(rows) != 1 || rows[0].State != store.StateDegraded {
				t.Fatalf("failed pass state: %v %v", rows, err)
			}
		})
	}
}

func TestSyncOnce_ThrottledMidPagination_CursorUnchanged(t *testing.T) {
	s, f, c, cfg := setup(t, mbA)
	f.Add(synth(mbA, 5)...)
	f.Throttle[mbA] = 1 // page 2 answers 429
	p := NewPoller(c, s, cfg.Mailboxes[0], cfg, nil)
	p.Top = 2
	_, _, err := p.SyncOnce(context.Background(), now0)
	var te *graph.ErrThrottled
	if !errors.As(err, &te) {
		t.Fatalf("want ErrThrottled, got %v", err)
	}
	if cur, _ := s.Cursor(context.Background(), mbA, "inbox"); !cur.IsZero() {
		t.Fatalf("cursor moved on a throttled read: %v", cur)
	}
	delete(f.Throttle, mbA)
	if _, _, err := p.SyncOnce(context.Background(), now0); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM message`); n != 5 {
		t.Fatalf("messages after recovery = %d, want 5", n)
	}
}

func TestSyncOnce_ForbiddenCursorUnchanged_SiblingContinues(t *testing.T) {
	s, f, c, cfg := setup(t, mbA, mbB)
	f.Add(synth(mbA, 2)...)
	f.Add(synth(mbB, 3)...)
	f.Forbidden[mbA] = true
	var pollers []*Poller
	for _, mb := range cfg.Mailboxes {
		pollers = append(pollers, NewPoller(c, s, mb, cfg, nil))
	}
	err := SyncAll(context.Background(), pollers, now0)
	var fe *graph.ErrMailboxForbidden
	if !errors.As(err, &fe) || fe.Mailbox != mbA {
		t.Fatalf("want the 403 of %s reported, got %v", mbA, err)
	}
	if n := count(t, s, `SELECT count(*) FROM message WHERE mailbox=?`, mbB); n != 3 {
		t.Fatalf("sibling synced %d of 3", n)
	}
	if cur, _ := s.Cursor(context.Background(), mbA, "inbox"); !cur.IsZero() {
		t.Fatalf("forbidden mailbox cursor moved: %v", cur)
	}
	rows, _ := s.ListMailboxes(context.Background())
	for _, r := range rows {
		if r.Address == mbA && (r.State != store.StateDegraded || r.Reason == "") {
			t.Errorf("forbidden mailbox not degraded: %+v", r)
		}
		if r.Address == mbB && r.State != store.StateOK {
			t.Errorf("healthy mailbox not ok: %+v", r)
		}
	}
}

func TestSyncOnce_ThreadKeyFromConversationOrSubject(t *testing.T) {
	s, f, c, cfg := setup(t, mbA)
	withConv := synth(mbA, 1)[0]
	noConvIn := graph.Message{ID: "g-x", InternetMessageID: "<x@a>", Mailbox: mbA, Folder: "inbox",
		Subject: "RE: Fwd:  Rinnovo Contratto!", From: "Mario@Client.Example", ReceivedAt: now0.Add(-30 * time.Minute)}
	noConvOut := graph.Message{ID: "g-y", InternetMessageID: "<y@a>", Mailbox: mbA, Folder: "sentitems",
		Subject: "R: rinnovo contratto", From: mbA, To: []string{"mario@client.example"}, ReceivedAt: now0.Add(-20 * time.Minute)}
	f.Add(withConv, noConvIn, noConvOut)
	if _, _, err := NewPoller(c, s, cfg.Mailboxes[0], cfg, nil).SyncOnce(context.Background(), now0); err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	rows, _ := s.DB().Query(`SELECT internet_message_id, thread_key FROM message`)
	for rows.Next() {
		var id, k string
		_ = rows.Scan(&id, &k)
		keys[id] = k
	}
	rows.Close()
	if keys[withConv.InternetMessageID] != "c:"+withConv.ConversationID {
		t.Errorf("conversation key = %q", keys[withConv.InternetMessageID])
	}
	if keys["<x@a>"] != "s:rinnovo contratto|mario@client.example" {
		t.Errorf("subject key = %q", keys["<x@a>"])
	}
	if keys["<x@a>"] != keys["<y@a>"] {
		t.Errorf("reply from us must share the thread: in=%q out=%q", keys["<x@a>"], keys["<y@a>"])
	}
}
