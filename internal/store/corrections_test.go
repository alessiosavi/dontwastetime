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
