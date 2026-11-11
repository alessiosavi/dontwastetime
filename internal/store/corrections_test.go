package store

import (
	"context"
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
	if err := s.AddCorrection(ctx, id, "unsure", "clienti", "founder", now0.Add(time.Hour)); err != nil {
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
	if err := s.AddCorrection(ctx, 999, "a", "b", "x", now0); err == nil {
		t.Fatal("correction of unknown message accepted")
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
		if err := s.AddCorrection(ctx, id, "accounting", "admin.requests", "owner", now0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		check()
		// Reclassification still replaces the flags and reply decision, including with NULL.
		reply = nil
		c.ReplyExpected = nil
		c.Urgent, c.Negative = false, false
		if err := s.SaveClassification(ctx, c, now0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		check()
	}
}
