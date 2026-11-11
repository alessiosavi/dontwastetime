package threads

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

func triageRow(t *testing.T, e *env) store.ThreadRow {
	t.Helper()
	r, err := e.s.Thread(context.Background(), "c:T")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func applyTriage(t *testing.T, e *env, now time.Time, change func(*sql.Tx) error) Status {
	t.Helper()
	st, err := Triage(context.Background(), e.s, e.cfg, "c:T", now, change)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func assertNoAck(t *testing.T, r store.ThreadRow) {
	t.Helper()
	if r.AckKind != "" || r.AckInboundAt != nil || r.AckInboundIMID != "" || r.SnoozedUntil != nil {
		t.Fatalf("ack not cleared: %+v", r)
	}
}

func TestMarkHandled_ClosesCurrentObligationOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
	now := t0.Add(30 * time.Hour)
	if st := e.status(t, "c:T", now); !st.NeedsReply {
		t.Fatal("test requires an overdue obligation")
	}
	st := applyTriage(t, e, now, func(tx *sql.Tx) error {
		return store.MarkHandledTx(ctx, tx, "c:T", id, "operator", "called client", now)
	})
	r := triageRow(t, e)
	if StateOf(r, now) != "handled" || st.NeedsReply || !st.DueAt.IsZero() || st.Priority != "" ||
		r.NeedsReply || r.ReplyDueAt != nil || r.Priority != "" || r.AckInboundAt == nil || !r.AckInboundAt.Equal(t0) {
		t.Fatalf("handled obligation: status=%+v row=%+v", st, r)
	}
	m, err := e.s.Message(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.AckInboundIMID != m.InternetMessageID {
		t.Fatalf("watermark imid=%q, want %q", r.AckInboundIMID, m.InternetMessageID)
	}
	var by, at, actor, action, target, note string
	err = e.s.DB().QueryRow(`SELECT t.ack_by, t.ack_at, a.actor, a.action, a.target,
		json_extract(a.details_json, '$.note') FROM thread t JOIN action_log a ON a.target = 'thread:' || t.thread_key`).Scan(
		&by, &at, &actor, &action, &target, &note)
	if err != nil || by != "operator" || at != now.UTC().Format("2006-01-02T15:04:05.000Z") || actor != by ||
		action != "mark_handled" || target != "thread:c:T" || note != "called client" {
		t.Fatalf("audit: by=%q at=%q actor=%q action=%q target=%q note=%q err=%v", by, at, actor, action, target, note, err)
	}
	if rows, err := e.s.ListNeedsReply(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("handled visible in needs-reply: rows=%v err=%v", rows, err)
	}
	e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: now})
	e.status(t, "c:T", now.Add(25*time.Hour))
	r = triageRow(t, e)
	if StateOf(r, now.Add(25*time.Hour)) != "overdue" {
		t.Fatalf("new inbound did not reopen: %+v", r)
	}
	assertNoAck(t, r)
}

func TestMarkHandled_OtherCopyAndOlderBackfillStayHandled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
	now := t0.Add(30 * time.Hour)
	e.status(t, "c:T", now)
	m, err := e.s.Message(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	copy := m.Graph()
	copy.Mailbox, copy.ID = "person.one@example.com", "other-copy"
	copyID, _, err := e.s.UpsertMessage(ctx, copy, "c:T", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.SaveClassification(ctx, store.Classification{MessageID: copyID, Label: "clienti", Method: "llm"}, now); err != nil {
		t.Fatal(err)
	}
	e.status(t, "c:T", now) // the other mailbox copy is now the last inbound row
	applyTriage(t, e, now, func(tx *sql.Tx) error {
		return store.MarkHandledTx(ctx, tx, "c:T", id, "operator", "", now)
	})
	// A third copy arrives after acknowledgement. Its numeric id is newer.
	copy.Mailbox, copy.ID = "employee@example.com", "late-copy"
	if _, _, err := e.s.UpsertMessage(ctx, copy, "c:T", now); err != nil {
		t.Fatal(err)
	}
	e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0.Add(-time.Hour), insertedAt: now})
	st := e.status(t, "c:T", now)
	r := triageRow(t, e)
	if StateOf(r, now) != "handled" || st.NeedsReply || r.AckInboundIMID != m.InternetMessageID {
		t.Fatalf("mailbox copy or older backfill reopened handled mail: status=%+v row=%+v", st, r)
	}
}

func TestMarkHandled_StaleInboundRefused(t *testing.T) {
	for _, delta := range []time.Duration{0, time.Hour} {
		for _, snooze := range []bool{false, true} {
			t.Run(delta.String()+map[bool]string{false: "/handled", true: "/snoozed"}[snooze], func(t *testing.T) {
				e := newEnv(t)
				ctx := context.Background()
				id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
				now := t0.Add(30 * time.Hour)
				e.status(t, "c:T", now)
				e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0.Add(delta)})
				e.status(t, "c:T", now)
				before := triageRow(t, e)
				_, err := Triage(ctx, e.s, e.cfg, "c:T", now, func(tx *sql.Tx) error {
					if snooze {
						return store.SnoozeTx(ctx, tx, "c:T", id, now.Add(time.Hour), "operator", now)
					}
					return store.MarkHandledTx(ctx, tx, "c:T", id, "operator", "", now)
				})
				if !errors.Is(err, store.ErrStale) {
					t.Fatalf("stale inbound: got %v, want ErrStale", err)
				}
				if after := triageRow(t, e); !reflect.DeepEqual(before, after) {
					t.Fatalf("stale request changed thread: before=%+v after=%+v", before, after)
				}
				var n int
				if err := e.s.DB().QueryRow("SELECT count(*) FROM action_log").Scan(&n); err != nil || n != 0 {
					t.Fatalf("stale request logged: count=%d err=%v", n, err)
				}
			})
		}
	}
}

func TestSnooze_HidesUntilThenReturnsWithOriginalDue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
	now := t0.Add(30 * time.Hour)
	until := now.Add(3 * 24 * time.Hour)
	e.status(t, "c:T", now)
	due := *triageRow(t, e).ReplyDueAt
	st := applyTriage(t, e, now, func(tx *sql.Tx) error {
		return store.SnoozeTx(ctx, tx, "c:T", id, until, "operator", now)
	})
	for _, at := range []time.Time{now, until.Add(-time.Millisecond)} {
		e.status(t, "c:T", at)
		r := triageRow(t, e)
		if StateOf(r, at) != "snoozed" || r.NeedsReply || r.Priority != "" || r.ReplyDueAt == nil || !r.ReplyDueAt.Equal(due) ||
			r.SnoozedUntil == nil || !r.SnoozedUntil.Equal(until) {
			t.Fatalf("snoozed row at %v: %+v", at, r)
		}
	}
	if st.NeedsReply || st.Priority != "" || !st.DueAt.Equal(due) {
		t.Fatalf("snoozed status: %+v", st)
	}
	if rows, err := e.s.ListNeedsReply(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("snoozed visible in needs-reply: rows=%v err=%v", rows, err)
	}
	e.status(t, "c:T", until)
	r := triageRow(t, e)
	if StateOf(r, until) != "overdue" || r.ReplyDueAt == nil || !r.ReplyDueAt.Equal(due) {
		t.Fatalf("expired snooze did not restore original obligation: %+v", r)
	}
	assertNoAck(t, r)
}

func TestSnooze_NewInboundEndsSnooze(t *testing.T) {
	for _, delta := range []time.Duration{0, 31 * time.Hour} {
		t.Run(delta.String(), func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			id := e.add(t, mopt{from: "a@unknown.example", label: "clienti", at: t0})
			now := t0.Add(30 * time.Hour)
			e.status(t, "c:T", now)
			applyTriage(t, e, now, func(tx *sql.Tx) error {
				return store.SnoozeTx(ctx, tx, "c:T", id, now.Add(3*24*time.Hour), "operator", now)
			})
			e.add(t, mopt{from: "a@unknown.example", label: "clienti", at: t0.Add(delta)})
			e.status(t, "c:T", now.Add(2*time.Hour))
			r := triageRow(t, e)
			want := "due"
			if delta == 0 {
				want = "overdue"
			}
			if StateOf(r, now.Add(2*time.Hour)) != want {
				t.Fatalf("new inbound did not end snooze: %+v", r)
			}
			assertNoAck(t, r)
		})
	}
}

func TestSweep_PicksUpExpiredSnoozeOutsideWindow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
	now := t0.Add(40 * 24 * time.Hour)
	until := t0.Add(45 * 24 * time.Hour)
	e.status(t, "c:T", now)
	applyTriage(t, e, now, func(tx *sql.Tx) error {
		return store.SnoozeTx(ctx, tx, "c:T", id, until, "operator", now)
	})
	now = t0.Add(46 * 24 * time.Hour)
	alerts := 0
	if err := Sweep(ctx, e.s, e.cfg, func(context.Context, []store.ThreadRow) error { alerts++; return nil }, now); err != nil {
		t.Fatal(err)
	}
	r := triageRow(t, e)
	if StateOf(r, now) != "overdue" || r.ReplyDueAt == nil || !r.ReplyDueAt.Equal(t0.Add(24*time.Hour)) || alerts != 1 {
		t.Fatalf("expired snooze outside sweep window: row=%+v alerts=%d", r, alerts)
	}
	assertNoAck(t, r)
}

func TestReopen_ClearsMuteAndAck(t *testing.T) {
	for _, snooze := range []bool{false, true} {
		t.Run(map[bool]string{false: "handled", true: "snoozed"}[snooze], func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
			now := t0.Add(30 * time.Hour)
			e.status(t, "c:T", now)
			applyTriage(t, e, now, func(tx *sql.Tx) error {
				if snooze {
					return store.SnoozeTx(ctx, tx, "c:T", id, now.Add(time.Hour), "operator", now)
				}
				return store.MarkHandledTx(ctx, tx, "c:T", id, "operator", "", now)
			})
			applyTriage(t, e, now, func(tx *sql.Tx) error {
				return store.MuteThreadTx(ctx, tx, "c:T", "operator", now)
			})
			if StateOf(triageRow(t, e), now) != "muted" {
				t.Fatal("mute must take precedence over ack")
			}
			applyTriage(t, e, now, func(tx *sql.Tx) error {
				return store.ReopenTx(ctx, tx, "c:T", "operator", now)
			})
			r := triageRow(t, e)
			if StateOf(r, now) != "overdue" || r.MutedAt != nil {
				t.Fatalf("reopen left thread silenced: %+v", r)
			}
			assertNoAck(t, r)
			var mutedBy, ackBy string
			var ackAt sql.NullString
			if err := e.s.DB().QueryRow("SELECT muted_by, ack_by, ack_at FROM thread WHERE thread_key = 'c:T'").Scan(&mutedBy, &ackBy, &ackAt); err != nil || mutedBy != "" || ackBy != "" || ackAt.Valid {
				t.Fatalf("reopen left attribution: muted_by=%q ack_by=%q ack_at=%v err=%v", mutedBy, ackBy, ackAt, err)
			}
			for _, action := range []string{map[bool]string{false: "mark_handled", true: "snooze"}[snooze], "mute_thread", "reopen"} {
				var n int
				if err := e.s.DB().QueryRow("SELECT count(*) FROM action_log WHERE actor = 'operator' AND target = 'thread:c:T' AND action = ?", action).Scan(&n); err != nil || n != 1 {
					t.Fatalf("audit action=%q count=%d err=%v", action, n, err)
				}
			}
		})
	}
}

func TestClaimP0Alert_SkipsHandledOrSnoozed(t *testing.T) {
	for _, kind := range []string{"handled", "snoozed", "muted", "not overdue", "not P0", "eligible"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
			now := t0.Add(30 * time.Hour)
			e.status(t, "c:T", now)
			applyTriage(t, e, now, func(tx *sql.Tx) error {
				switch kind {
				case "handled":
					return store.MarkHandledTx(ctx, tx, "c:T", id, "operator", "", now)
				case "snoozed":
					return store.SnoozeTx(ctx, tx, "c:T", id, now.Add(time.Hour), "operator", now)
				case "muted":
					return store.MuteThreadTx(ctx, tx, "c:T", "operator", now)
				}
				return nil
			})
			// Retain stale computed eligibility to isolate the claim's own checks.
			needs, priority := true, PriorityP0
			if kind == "not overdue" {
				needs = false
			}
			if kind == "not P0" {
				priority = PriorityP1
			}
			if _, err := e.s.DB().Exec("UPDATE thread SET needs_reply = ?, priority = ?, p0_alerted_day = 'previous' WHERE thread_key = 'c:T'", needs, priority); err != nil {
				t.Fatal(err)
			}
			claimed, prev, err := e.s.ClaimP0Alert(ctx, "c:T", "2026-11-11")
			if err != nil || claimed != (kind == "eligible") || prev != "previous" {
				t.Fatalf("claim=%v prev=%q err=%v", claimed, prev, err)
			}
			wantDay := "previous"
			if kind == "eligible" {
				wantDay = "2026-11-11"
			}
			if r := triageRow(t, e); r.P0AlertedDay != wantDay {
				t.Fatalf("claim marker=%q, want %q", r.P0AlertedDay, wantDay)
			}
		})
	}
}

func TestTriage_HandledSurvivesConcurrentRecompute(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
	now := t0.Add(30 * time.Hour)
	e.status(t, "c:T", now)
	var path string
	var seq int
	var name string
	if err := e.s.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	other, err := store.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	started, done := make(chan struct{}), make(chan error, 1)
	beforeSave = func() {
		beforeSave = nil
		go func() {
			close(started)
			_, err := Triage(ctx, other, e.cfg, "c:T", now, func(tx *sql.Tx) error {
				return store.MarkHandledTx(ctx, tx, "c:T", id, "operator", "", now)
			})
			done <- err
		}()
		<-started
	}
	defer func() { beforeSave = nil }()
	if _, err := Recompute(ctx, e.s, "c:T", e.cfg, now); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("triage did not complete after concurrent recompute")
	}
	e.status(t, "c:T", now)
	if r := triageRow(t, e); StateOf(r, now) != "handled" || r.NeedsReply {
		t.Fatalf("concurrent recompute lost handled state: %+v", r)
	}
}

func TestTriage_UnknownThreadOrMessage(t *testing.T) {
	for _, action := range []string{"mark_handled", "snooze", "mute_thread", "reopen"} {
		t.Run(action, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
			e.status(t, "c:T", t0)
			for _, key := range []string{"c:missing", "c:T"} {
				if key == "c:T" && (action == "mute_thread" || action == "reopen") {
					continue
				}
				inboundID := id
				if key == "c:T" {
					inboundID = id + 100
				}
				_, err := Triage(ctx, e.s, e.cfg, key, t0, func(tx *sql.Tx) error {
					switch action {
					case "mark_handled":
						return store.MarkHandledTx(ctx, tx, key, inboundID, "operator", "", t0)
					case "snooze":
						return store.SnoozeTx(ctx, tx, key, inboundID, t0.Add(time.Hour), "operator", t0)
					case "mute_thread":
						return store.MuteThreadTx(ctx, tx, key, "operator", t0)
					default:
						return store.ReopenTx(ctx, tx, key, "operator", t0)
					}
				})
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("unknown key=%q inbound=%d: got %v, want ErrNotFound", key, inboundID, err)
				}
			}
		})
	}
}

func TestTriage_RecomputeFailureRollsBackChangeAndAudit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
	e.status(t, "c:T", t0)
	before := triageRow(t, e)
	if _, err := e.s.DB().Exec(`CREATE TRIGGER fail_thread_save BEFORE UPDATE OF subject ON thread
		BEGIN SELECT RAISE(ABORT, 'forced recompute failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := Triage(ctx, e.s, e.cfg, "c:T", t0, func(tx *sql.Tx) error {
		return store.MarkHandledTx(ctx, tx, "c:T", id, "operator", "", t0)
	})
	if err == nil {
		t.Fatal("forced recompute failure was ignored")
	}
	if after := triageRow(t, e); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed recompute committed change: before=%+v after=%+v", before, after)
	}
	var n int
	if err := e.s.DB().QueryRow("SELECT count(*) FROM action_log").Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed recompute committed audit: count=%d err=%v", n, err)
	}
}

func TestSnooze_RejectsInvalidUntil(t *testing.T) {
	for _, offset := range []time.Duration{-time.Hour, 0, 90*24*time.Hour + time.Millisecond} {
		t.Run(offset.String(), func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			id := e.add(t, mopt{from: "a@bigbrand.example", label: "clienti", at: t0})
			e.status(t, "c:T", t0)
			_, err := Triage(ctx, e.s, e.cfg, "c:T", t0, func(tx *sql.Tx) error {
				return store.SnoozeTx(ctx, tx, "c:T", id, t0.Add(offset), "operator", t0)
			})
			if err == nil {
				t.Fatal("invalid snooze accepted")
			}
			assertNoAck(t, triageRow(t, e))
		})
	}
}

func TestStateOf_TriagePrecedenceAndDue(t *testing.T) {
	future := t0.Add(time.Hour)
	for _, tc := range []struct {
		name string
		row  store.ThreadRow
		want string
	}{
		{"empty", store.ThreadRow{}, "none"},
		{"due", store.ThreadRow{ReplyDueAt: &future}, "due"},
		{"overdue", store.ThreadRow{NeedsReply: true, ReplyDueAt: &future}, "overdue"},
		{"handled", store.ThreadRow{AckKind: "handled", ReplyDueAt: &future, NeedsReply: true}, "handled"},
		{"snoozed", store.ThreadRow{AckKind: "snoozed", SnoozedUntil: &future, ReplyDueAt: &t0, NeedsReply: true}, "snoozed"},
		{"expired", store.ThreadRow{AckKind: "snoozed", SnoozedUntil: &t0, ReplyDueAt: &t0, NeedsReply: true}, "overdue"},
		{"muted", store.ThreadRow{MutedAt: &t0, AckKind: "handled", ReplyDueAt: &future}, "muted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StateOf(tc.row, t0); got != tc.want {
				t.Fatalf("state=%q, want %q", got, tc.want)
			}
		})
	}
}
