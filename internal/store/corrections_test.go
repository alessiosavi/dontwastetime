package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestCorrections_Roundtrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id, _ := upsert(t, s, msg("m@example.test", "<c@x>", now0))
	if err := s.SaveClassification(ctx, Classification{MessageID: id, Label: "unsure", Confidence: 0.4, Method: "llm"}, now0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddCorrection(ctx, id, "clienti", "founder", now0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecentCorrections(ctx, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("RecentCorrections: %+v %v", got, err)
	}
	c := got[0]
	if c.MessageID != id || c.OldLabel != "unsure" || c.NewLabel != "clienti" || c.By != "founder" || c.Subject != "Oggetto <c@x>" || c.From != "mario@client.example" {
		t.Fatalf("correction: %+v", c)
	}
	cl, err := s.ClassificationOf(ctx, id)
	if err != nil || cl.Label != "clienti" || cl.Method != "correction" || cl.Confidence != 1 {
		t.Fatalf("classification after correction: %+v %v", cl, err)
	}
	if old, err := s.AddCorrection(ctx, id, "business.dev", "founder", now0.Add(2*time.Hour)); err != nil || old != "clienti" {
		t.Fatalf("second correction: old=%q, err=%v", old, err)
	}
	cl, err = s.ClassificationOf(ctx, id)
	if err != nil || cl.Label != "business.dev" || cl.Method != "correction" {
		t.Fatalf("classification after second correction: %+v %v", cl, err)
	}
	if _, err := s.AddCorrection(ctx, 999, "b", "x", now0); err == nil {
		t.Fatal("correction of unknown message accepted")
	}
}

func TestSaveClassification_LLMDoesNotOverwriteCorrection(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id, _ := upsert(t, s, msg("m@example.test", "<late@x>", now0))
	if _, err := s.AddCorrection(ctx, id, "clienti", "owner", now0); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveClassification(ctx, Classification{MessageID: id, Label: "marketing.news", Method: "llm"}, now0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cl, err := s.ClassificationOf(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Label != "clienti" || cl.Method != "correction" {
		t.Fatalf("late LLM overwrote correction: label=%q method=%q", cl.Label, cl.Method)
	}
}

func TestAddCorrection_ReturnsOldLabelAndEnqueuesRefresh(t *testing.T) {
	for _, oldLabel := range []string{"business.dev", ""} {
		t.Run("old="+oldLabel, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			id, _ := upsert(t, s, msg("m@example.test", "<refresh@x>", now0))
			if oldLabel != "" {
				if err := s.SaveClassification(ctx, Classification{MessageID: id, Label: oldLabel, Method: "llm"}, now0); err != nil {
					t.Fatal(err)
				}
			}
			old, err := s.AddCorrection(ctx, id, "clienti", "owner", now0.Add(time.Hour))
			if err != nil || old != oldLabel {
				t.Fatalf("AddCorrection: old=%q, err=%v, want %q", old, err, oldLabel)
			}
			counts, err := s.JobCounts(ctx, KindThreadRefresh, id)
			if err != nil {
				t.Fatal(err)
			}
			if counts["pending"] != 1 {
				t.Errorf("pending thread refresh jobs=%d, want 1", counts["pending"])
			}
			var actor, target, auditOld, label string
			if err := s.DB().QueryRowContext(ctx, `SELECT actor, target, json_extract(details_json, '$.old'),
				json_extract(details_json, '$.new') FROM action_log WHERE action = 'correct'`).Scan(&actor, &target, &auditOld, &label); err != nil {
				t.Fatal(err)
			}
			if actor != "owner" || target != "message:"+strconv.FormatInt(id, 10) || auditOld != oldLabel || label != "clienti" {
				t.Fatalf("audit: actor=%q target=%q old=%q new=%q", actor, target, auditOld, label)
			}
			var n int
			if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM action_log WHERE action = 'correct'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("correct audit rows=%d, err=%v, want 1", n, err)
			}
		})
	}
}

func TestAddCorrection_RollsBackIfRefreshEnqueueFails(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id, _ := upsert(t, s, msg("m@example.test", "<rollback@x>", now0))
	if err := s.SaveClassification(ctx, Classification{MessageID: id, Label: "business.dev", Method: "llm"}, now0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `CREATE TRIGGER fail_refresh BEFORE INSERT ON queue
		WHEN NEW.kind = 'thread_refresh' BEGIN SELECT RAISE(ABORT, 'refresh enqueue failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddCorrection(ctx, id, "clienti", "owner", now0.Add(time.Hour)); err == nil {
		t.Fatal("correction succeeded despite refresh enqueue failure")
	}
	cl, err := s.ClassificationOf(ctx, id)
	if err != nil || cl.Label != "business.dev" || cl.Method != "llm" {
		t.Fatalf("classification changed after rollback: %+v, err=%v", cl, err)
	}
	for _, table := range []string{"corrections", "action_log", "queue"} {
		var n int
		if err := s.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s rows=%d, err=%v, want 0 after rollback", table, n, err)
		}
	}
}

func TestMuteThread(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.DB().Exec(`INSERT INTO thread(thread_key, mailbox) VALUES ('c:1', 'm@example.test')`); err != nil {
		t.Fatal(err)
	}
	if err := s.MuteThread(ctx, "c:1", "founder", now0); err != nil {
		t.Fatal(err)
	}
	var mutedBy string
	var mutedAt *string
	_ = s.DB().QueryRow(`SELECT muted_by, muted_at FROM thread WHERE thread_key='c:1'`).Scan(&mutedBy, &mutedAt)
	if mutedBy != "founder" || mutedAt == nil {
		t.Fatalf("muted_by=%q muted_at=%v", mutedBy, mutedAt)
	}
	if err := s.MuteThread(ctx, "c:missing", "founder", now0); err == nil {
		t.Fatal("muting an unknown thread accepted")
	}
}

func TestClassification_ReplyExpectedRoundtripAndCorrection(t *testing.T) {
	for _, reply := range []*bool{nil, new(true), new(false)} {
		s := openTest(t)
		ctx := context.Background()
		id, _ := upsert(t, s, msg("m@example.test", "<reply@x>", now0))
		c := Classification{MessageID: id, Label: "accounting", Confidence: 0.9, Method: "llm",
			Urgent: true, Negative: true, ReplyExpected: reply}
		if err := s.SaveClassification(ctx, c, now0); err != nil {
			t.Fatal(err)
		}
		check := func() {
			t.Helper()
			cl, err := s.ClassificationOf(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			messages, err := s.ThreadMessages(ctx, "tk-<reply@x>")
			if err != nil || len(messages) != 1 {
				t.Fatalf("messages=%+v, err=%v", messages, err)
			}
			if cl.Urgent != c.Urgent || cl.Negative != c.Negative ||
				messages[0].Urgent != c.Urgent || messages[0].Negative != c.Negative {
				t.Fatalf("flags: classification=%+v, message=%+v", cl, messages[0])
			}
			for _, got := range []*bool{cl.ReplyExpected, messages[0].ReplyExpected} {
				if (got == nil) != (reply == nil) || (got != nil && *got != *reply) {
					t.Fatalf("reply_expected=%v, want %v", got, reply)
				}
			}
		}
		check()
		// Reclassification still replaces the flags and reply decision, including with NULL.
		original := c
		reply = nil
		c.ReplyExpected = nil
		c.Urgent, c.Negative = false, false
		if err := s.SaveClassification(ctx, c, now0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		check()
		// Restore the original flags before correcting, then ignore late classifier results.
		late := c
		c, reply = original, original.ReplyExpected
		if err := s.SaveClassification(ctx, c, now0.Add(3*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddCorrection(ctx, id, "admin.requests", "owner", now0.Add(4*time.Hour)); err != nil {
			t.Fatal(err)
		}
		check()
		if err := s.SaveClassification(ctx, late, now0.Add(5*time.Hour)); err != nil {
			t.Fatal(err)
		}
		check()
	}
}
