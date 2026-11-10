package threads

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

var t0 = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

type env struct {
	s   *store.Store
	cfg *config.Config
	n   int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "dwt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cfg := &config.Config{
		Mailboxes: []config.MailboxConfig{
			{Address: "business@example.com", Enabled: true, Digest: true, PollSeconds: 180},
			{Address: "person.one@example.com", Enabled: true, Digest: true, PollSeconds: 180},
			{Address: "employee@example.com", Enabled: true, Digest: false, PollSeconds: 180},
		},
		TopClients: []string{"bigbrand.example"},
	}
	return &env{s: s, cfg: cfg}
}

type mopt struct {
	thread, mailbox, folder, from, subject, body, label string
	at                                                  time.Time
	urgent, negative, auto                              bool
}

// add stores one classified message and returns its row id.
func (e *env) add(t *testing.T, o mopt) int64 {
	t.Helper()
	e.n++
	if o.mailbox == "" {
		o.mailbox = "business@example.com"
	}
	if o.folder == "" {
		o.folder = "inbox"
	}
	if o.thread == "" {
		o.thread = "c:T"
	}
	m := graph.Message{ID: "g" + string(rune('a'+e.n)), InternetMessageID: "<" + string(rune('a'+e.n)) + "@x>",
		Mailbox: o.mailbox, Folder: o.folder, From: o.from, Subject: o.subject, BodyText: o.body, BodyPreview: o.body,
		ReceivedAt: o.at}
	id, _, err := e.s.UpsertMessage(context.Background(), m, o.thread, o.at)
	if err != nil {
		t.Fatal(err)
	}
	if o.auto {
		_ = e.s.UpdateMessageFlags(context.Background(), id, true, false, false)
	}
	if o.label != "" {
		if err := e.s.SaveClassification(context.Background(), store.Classification{MessageID: id, Label: o.label,
			Confidence: 0.9, Urgent: o.urgent, Negative: o.negative, Method: "llm"}, o.at); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (e *env) status(t *testing.T, key string, now time.Time) Status {
	t.Helper()
	st, err := Recompute(context.Background(), e.s, key, e.cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestNeedsReply_LastInboundAndAge(t *testing.T) {
	cases := []struct {
		label string
		age   time.Duration
		want  bool
	}{
		{"clienti", 23*time.Hour + 55*time.Minute, false},
		{"clienti", 24*time.Hour + 5*time.Minute, true},
		{"business.dev", 24*time.Hour + 5*time.Minute, true},
		{"ops.spaces", 47 * time.Hour, false},
		{"ops.spaces", 49 * time.Hour, true},
		{"eventi.post", 49 * time.Hour, true},
		{"admin.requests", 49 * time.Hour, true},
	}
	for _, c := range cases {
		e := newEnv(t)
		e.add(t, mopt{from: "mario@unknown.example", subject: "Domanda", label: c.label, at: t0})
		st := e.status(t, "c:T", t0.Add(c.age))
		if st.NeedsReply != c.want {
			t.Errorf("%s at %s: needs reply = %v, want %v", c.label, c.age, st.NeedsReply, c.want)
		}
		if c.want && st.Cassetto != c.label {
			t.Errorf("cassetto = %q", st.Cassetto)
		}
	}
	e := newEnv(t)
	e.add(t, mopt{from: "mario@unknown.example", label: "clienti", at: t0})
	if st := e.status(t, "c:T", t0.Add(30*time.Hour)); !st.DueAt.Equal(t0.Add(24 * time.Hour)) {
		t.Errorf("due at %v, want %v", st.DueAt, t0.Add(24*time.Hour))
	}
}

func TestNeedsReply_Suppressions(t *testing.T) {
	cases := map[string]mopt{
		"no-reply sender": {from: "no-reply@client.example", subject: "Aggiornamento"},
		"mailer-daemon":   {from: "mailer-daemon@client.example", subject: "Undeliverable"},
		"pec receipt":     {from: "posta@pec.client.example", subject: "ACCETTAZIONE: Contratto"},
		"oof":             {from: "mario@client.example", subject: "Risposta automatica: Contratto"},
		"calendar reply":  {from: "mario@client.example", subject: "Accettato: Riunione rinnovo"},
		"auto flag":       {from: "mario@client.example", subject: "Contratto", auto: true},
	}
	for name, o := range cases {
		e := newEnv(t)
		o.label, o.at = "clienti", t0
		e.add(t, o)
		if st := e.status(t, "c:T", t0.Add(30*time.Hour)); st.NeedsReply {
			t.Errorf("%s: flagged as needing a reply", name)
		}
	}
	e := newEnv(t)
	e.add(t, mopt{from: "mario@client.example", subject: "Contratto", label: "clienti", at: t0})
	e.status(t, "c:T", t0.Add(time.Hour))
	if err := e.s.MuteThread(context.Background(), "c:T", "founder", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := e.status(t, "c:T", t0.Add(30*time.Hour)); st.NeedsReply {
		t.Error("muted thread flagged")
	}
}

func TestNeedsReply_OtherLabelsNever(t *testing.T) {
	for _, label := range []string{"admin.invoices", "accounting", "hr.recruiting", "notifications", "marketing.news", "unsure"} {
		e := newEnv(t)
		e.add(t, mopt{from: "x@vendor.example", subject: "Fattura", label: label, at: t0})
		if st := e.status(t, "c:T", t0.Add(100*time.Hour)); st.NeedsReply {
			t.Errorf("%s flagged as needing a reply", label)
		}
	}
}

func TestNeedsReply_ReplyFromAnyTrackedMailboxClears(t *testing.T) {
	e := newEnv(t)
	e.add(t, mopt{from: "mario@client.example", subject: "Contratto", label: "clienti", at: t0})
	e.add(t, mopt{mailbox: "person.one@example.com", folder: "sentitems", from: "person.one@example.com", subject: "Re: Contratto", label: "clienti", at: t0.Add(time.Hour)})
	if st := e.status(t, "c:T", t0.Add(30*time.Hour)); st.NeedsReply {
		t.Error("reply from another tracked mailbox did not clear the thread")
	}
	// A colleague's answer seen inbound (internal domain, cc) also counts as ours.
	e2 := newEnv(t)
	e2.add(t, mopt{from: "mario@client.example", subject: "Contratto", label: "clienti", at: t0})
	e2.add(t, mopt{from: "collega@example.com", subject: "Re: Contratto", label: "clienti", at: t0.Add(time.Hour)})
	if st := e2.status(t, "c:T", t0.Add(30*time.Hour)); st.NeedsReply {
		t.Error("internal colleague's reply did not clear the thread")
	}
	// A new inbound after our reply re-opens it.
	e.add(t, mopt{from: "mario@client.example", subject: "Re: Contratto", label: "clienti", at: t0.Add(2 * time.Hour)})
	if st := e.status(t, "c:T", t0.Add(30*time.Hour)); !st.NeedsReply {
		t.Error("new inbound after our reply must re-open the thread")
	}
}

func TestPriority_P0NeedsKnownDomain(t *testing.T) {
	cases := []struct {
		name string
		o    mopt
		age  time.Duration
		want string
	}{
		{"urgent words, unknown domain", mopt{from: "a@unknown.example", subject: "URGENTE rinnovo"}, time.Hour, "P1"},
		{"urgent words, top client", mopt{from: "a@bigbrand.example", subject: "Serve entro domani"}, time.Hour, "P0"},
		{"urgent flag, top client", mopt{from: "a@bigbrand.example", subject: "Contratto", urgent: true}, time.Hour, "P0"},
		{"negative flag, top client", mopt{from: "a@bigbrand.example", subject: "Contratto", negative: true}, time.Hour, "P0"},
		{"sla breach, top client", mopt{from: "a@bigbrand.example", subject: "Info"}, 25 * time.Hour, "P0"},
		{"question, unknown", mopt{from: "a@unknown.example", subject: "Info", body: "Possiamo sentirci per il preventivo?"}, time.Hour, "P1"},
		{"fyi, unknown", mopt{from: "a@unknown.example", subject: "Info", body: "Vi giro il materiale."}, time.Hour, "P2"},
	}
	for _, c := range cases {
		e := newEnv(t)
		c.o.label, c.o.at = "clienti", t0
		e.add(t, c.o)
		if st := e.status(t, "c:T", t0.Add(c.age)); st.Priority != c.want {
			t.Errorf("%s: priority %q, want %q", c.name, st.Priority, c.want)
		}
	}
	// Known by history: a classified clienti message in another thread.
	e := newEnv(t)
	e.add(t, mopt{thread: "c:old", from: "b@history.example", subject: "Ordine", label: "clienti", at: t0.Add(-72 * time.Hour)})
	e.add(t, mopt{from: "c@history.example", subject: "Bloccante: consegna", label: "clienti", at: t0})
	if st := e.status(t, "c:T", t0.Add(time.Hour)); st.Priority != "P0" {
		t.Errorf("domain with clienti history: priority %q, want P0", st.Priority)
	}
	// Answered threads never alert.
	e.add(t, mopt{folder: "sentitems", from: "business@example.com", subject: "Re", label: "clienti", at: t0.Add(30 * time.Minute)})
	if st := e.status(t, "c:T", t0.Add(time.Hour)); st.Priority == "P0" {
		t.Error("answered thread still P0")
	}
}

func TestPinnedOverSevenDays(t *testing.T) {
	e := newEnv(t)
	e.add(t, mopt{from: "a@unknown.example", subject: "Info", label: "clienti", at: t0})
	st := e.status(t, "c:T", t0.Add(8*24*time.Hour))
	if !st.NeedsReply || st.Priority != PriorityPinned {
		t.Fatalf("8-day-old client thread: %+v", st)
	}
	rows, err := e.s.ListNeedsReply(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Priority != PriorityPinned {
		t.Fatalf("ListNeedsReply: %+v %v", rows, err)
	}
}

func TestListNeedsReply_OrderAndMute(t *testing.T) {
	e := newEnv(t)
	e.add(t, mopt{thread: "c:p2", from: "a@unknown.example", subject: "Info", body: "ok", label: "clienti", at: t0})
	e.add(t, mopt{thread: "c:pin", from: "b@unknown.example", subject: "Info", label: "clienti", at: t0.Add(-8 * 24 * time.Hour)})
	e.add(t, mopt{thread: "c:p0", from: "c@bigbrand.example", subject: "Info", label: "clienti", at: t0})
	e.add(t, mopt{thread: "c:mute", from: "d@unknown.example", subject: "Info", label: "clienti", at: t0})
	now := t0.Add(26 * time.Hour)
	for _, k := range []string{"c:p2", "c:pin", "c:p0", "c:mute"} {
		e.status(t, k, now)
	}
	_ = e.s.MuteThread(context.Background(), "c:mute", "founder", now)
	rows, err := e.s.ListNeedsReply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range rows {
		keys = append(keys, r.ThreadKey)
	}
	if len(keys) != 3 || keys[0] != "c:pin" || keys[1] != "c:p0" || keys[2] != "c:p2" {
		t.Fatalf("order = %v, want [pinned, P0, P2] and muted excluded", keys)
	}
}

func TestRefreshHandler_AlertsOncePerThreadDay(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, mopt{from: "a@bigbrand.example", subject: "URGENTE consegna", label: "clienti", at: t0})
	var alerts int
	alert := func(_ context.Context, rows []store.ThreadRow) error {
		alerts += len(rows)
		return nil
	}
	now := t0.Add(time.Hour)
	h := RefreshHandler(e.s, e.cfg, alert, func() time.Time { return now })
	for range 2 {
		if err := h(context.Background(), &store.Job{Ref: id}); err != nil {
			t.Fatal(err)
		}
	}
	if alerts != 1 {
		t.Fatalf("alerts on the same day = %d, want 1", alerts)
	}
	now = t0.Add(25 * time.Hour) // next Rome day, still unanswered
	_ = h(context.Background(), &store.Job{Ref: id})
	if alerts != 2 {
		t.Fatalf("alerts after a new day = %d, want 2", alerts)
	}
}

func TestRefreshHandler_NoAlertForDigestOffMailbox(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, mopt{mailbox: "employee@example.com", from: "a@bigbrand.example", subject: "URGENTE", label: "clienti", at: t0})
	alerts := 0
	h := RefreshHandler(e.s, e.cfg, func(context.Context, []store.ThreadRow) error { alerts++; return nil },
		func() time.Time { return t0.Add(time.Hour) })
	if err := h(context.Background(), &store.Job{Ref: id}); err != nil {
		t.Fatal(err)
	}
	if alerts != 0 {
		t.Fatal("P0 alert for a digest:false mailbox")
	}
}

func TestSweep_AlertsOnSLABreach(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, mopt{from: "a@bigbrand.example", subject: "Info", body: "ok", label: "clienti", at: t0})
	alerts := 0
	alert := func(_ context.Context, rows []store.ThreadRow) error { alerts += len(rows); return nil }
	h := RefreshHandler(e.s, e.cfg, alert, func() time.Time { return t0.Add(time.Hour) })
	if err := h(context.Background(), &store.Job{Ref: id}); err != nil {
		t.Fatal(err)
	}
	if alerts != 0 {
		t.Fatal("alert before the SLA breach")
	}
	if err := Sweep(context.Background(), e.s, e.cfg, alert, t0.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if alerts != 1 {
		t.Fatalf("SLA breach alerts = %d, want 1", alerts)
	}
	_ = Sweep(context.Background(), e.s, e.cfg, alert, t0.Add(26*time.Hour))
	if alerts != 1 {
		t.Fatal("sweep re-alerted the same day")
	}
}
