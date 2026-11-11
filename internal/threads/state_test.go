package threads

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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
	at, insertedAt                                      time.Time
	to, cc                                              []string
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
		Mailbox: o.mailbox, Folder: o.folder, From: o.from, To: o.to, CC: o.cc, Subject: o.subject, BodyText: o.body, BodyPreview: o.body,
		ReceivedAt: o.at}
	if o.insertedAt.IsZero() {
		o.insertedAt = o.at
	}
	id, _, err := e.s.UpsertMessage(context.Background(), m, o.thread, o.insertedAt)
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

func TestNeedsReply_BacklogCutoff(t *testing.T) {
	for _, label := range []string{"clienti", "business.dev", "eventi.external"} {
		for _, tc := range []struct {
			name  string
			delay time.Duration
			open  bool
		}{
			{"fresh inserted one hour later", time.Hour, true},
			{"exactly fourteen days", 14 * 24 * time.Hour, true},
			{"over fourteen days", 14*24*time.Hour + time.Millisecond, false},
			{"fifteen-day backfill", 15 * 24 * time.Hour, false},
			{"seventy-seven-day backfill", 77 * 24 * time.Hour, false},
		} {
			t.Run(label+"/"+tc.name, func(t *testing.T) {
				e := newEnv(t)
				e.add(t, mopt{from: "a@bigbrand.example", subject: "URGENTE", label: label, at: t0, insertedAt: t0.Add(tc.delay)})
				st := e.status(t, "c:T", t0.Add(tc.delay+49*time.Hour))
				if st.NeedsReply != tc.open || (tc.open && st.DueAt.IsZero()) ||
					(!tc.open && (st.Priority != "" || !st.DueAt.IsZero())) {
					t.Fatalf("status %+v, want open=%v", st, tc.open)
				}
				row, err := e.s.Thread(context.Background(), "c:T")
				if err != nil || row.NeedsReply != tc.open || (!tc.open && (row.Priority != "" || row.ReplyDueAt != nil)) {
					t.Fatalf("persisted state %+v, err=%v; want open=%v", row, err, tc.open)
				}
				rows, err := e.s.ListNeedsReply(context.Background())
				if err != nil || (len(rows) == 1) != tc.open {
					t.Fatalf("needs-reply rows=%d, err=%v; want open=%v", len(rows), err, tc.open)
				}
			})
		}
	}
}

func TestNeedsReply_BacklogLastInboundClearsState(t *testing.T) {
	e := newEnv(t)
	e.add(t, mopt{from: "a@bigbrand.example", subject: "URGENTE", label: "clienti", at: t0.Add(-time.Hour)})
	if st := e.status(t, "c:T", t0.Add(time.Hour)); !st.NeedsReply || st.Priority != PriorityP0 {
		t.Fatalf("initial fresh inbound: %+v", st)
	}
	e.add(t, mopt{from: "a@bigbrand.example", subject: "Re: URGENTE", label: "clienti", at: t0, insertedAt: t0.Add(15 * 24 * time.Hour)})
	st := e.status(t, "c:T", t0.Add(16*24*time.Hour))
	if st.NeedsReply || st.Priority != "" || !st.DueAt.IsZero() {
		t.Fatalf("last inbound is backlog: %+v", st)
	}
	row, err := e.s.Thread(context.Background(), "c:T")
	if err != nil || row.NeedsReply || row.Priority != "" || row.ReplyDueAt != nil {
		t.Fatalf("stale obligation persisted: %+v, err=%v", row, err)
	}
}

func TestNeedsReply_BacklogFollowedByFreshInbound(t *testing.T) {
	e := newEnv(t)
	inserted := t0.Add(15 * 24 * time.Hour)
	e.add(t, mopt{from: "a@bigbrand.example", subject: "Progetto", label: "clienti", at: t0, insertedAt: inserted})
	if st := e.status(t, "c:T", inserted); st.NeedsReply || st.Priority != "" || !st.DueAt.IsZero() {
		t.Fatalf("backlog opened an obligation: %+v", st)
	}
	fresh := inserted.Add(time.Hour)
	e.add(t, mopt{from: "a@bigbrand.example", subject: "Re: Progetto", label: "clienti", at: fresh, insertedAt: fresh.Add(time.Hour)})
	st := e.status(t, "c:T", fresh.Add(25*time.Hour))
	if !st.NeedsReply || st.Priority != PriorityP0 || !st.DueAt.Equal(fresh.Add(24*time.Hour)) {
		t.Fatalf("fresh inbound failed to reopen: %+v", st)
	}
	row, err := e.s.Thread(context.Background(), "c:T")
	if err != nil || !row.NeedsReply || row.ReplyDueAt == nil || !row.ReplyDueAt.Equal(st.DueAt) {
		t.Fatalf("fresh obligation not persisted: %+v, err=%v", row, err)
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
		if st := e.status(t, "c:T", t0.Add(100*time.Hour)); st.NeedsReply || st.Priority != "" || !st.DueAt.IsZero() {
			t.Errorf("%s opened a reply obligation: %+v", label, st)
		}
	}
}

func TestNeedsReply_ReplyFromAnyTrackedMailboxClears(t *testing.T) {
	e := newEnv(t)
	e.add(t, mopt{from: "mario@client.example", subject: "Contratto", label: "clienti", at: t0})
	e.add(t, mopt{mailbox: "person.one@example.com", folder: "sentitems", from: "person.one@example.com", to: []string{"mario@client.example"}, subject: "Re: Contratto", label: "clienti", at: t0.Add(time.Hour)})
	if st := e.status(t, "c:T", t0.Add(30*time.Hour)); st.NeedsReply {
		t.Error("reply from another tracked mailbox did not clear the thread")
	}
	// A colleague's answer seen inbound (internal domain, cc) also counts as ours.
	e2 := newEnv(t)
	e2.add(t, mopt{from: "mario@client.example", subject: "Contratto", label: "clienti", at: t0})
	e2.add(t, mopt{from: "collega@example.com", to: []string{"mario@client.example"}, subject: "Re: Contratto", label: "clienti", at: t0.Add(time.Hour)})
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
	// Known by history: an outbound message in another thread, even unclassified.
	e := newEnv(t)
	e.add(t, mopt{thread: "c:old", folder: "sentitems", from: "business@example.com", to: []string{"b@history.example"}, subject: "Ordine", at: t0.Add(-72 * time.Hour)})
	e.add(t, mopt{from: "c@history.example", subject: "Bloccante: consegna", label: "clienti", at: t0})
	if st := e.status(t, "c:T", t0.Add(time.Hour)); st.Priority != "P0" {
		t.Errorf("domain with outbound history: priority %q, want P0", st.Priority)
	}
	// Answered threads never alert.
	e.add(t, mopt{folder: "sentitems", from: "business@example.com", to: []string{"c@history.example"}, subject: "Re", label: "clienti", at: t0.Add(30 * time.Minute)})
	if st := e.status(t, "c:T", t0.Add(time.Hour)); st.Priority == "P0" {
		t.Error("answered thread still P0")
	}
}

func TestPriority_KnownClientRequiresEarlierOutbound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history mopt
		want    string
	}{
		{"outbound To", mopt{to: []string{"other@example.test", "B@PARTNER.EXAMPLE"}}, PriorityP0},
		{"outbound Cc", mopt{to: []string{"other@example.test"}, cc: []string{"B@PARTNER.EXAMPLE"}}, PriorityP0},
		{"inbound clienti alone", mopt{folder: "inbox", from: "b@partner.example", label: "clienti"}, PriorityP1},
		{"near-miss domain", mopt{to: []string{"b@notpartner.example"}}, PriorityP1},
		{"recipient subdomain", mopt{cc: []string{"b@mail.partner.example"}}, PriorityP1},
		{"outbound after inbound", mopt{to: []string{"b@partner.example"}, at: t0.Add(time.Hour)}, PriorityP1},
		{"outbound simultaneous", mopt{to: []string{"b@partner.example"}, at: t0}, PriorityP1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			h := tc.history
			h.thread = "c:history"
			if h.folder == "" {
				h.folder, h.from = "sentitems", "business@example.com"
			}
			if h.at.IsZero() {
				h.at = t0.Add(-time.Hour)
			}
			e.add(t, h)
			e.add(t, mopt{from: "a@partner.example", subject: "URGENTE", label: "clienti", at: t0})
			if st := e.status(t, "c:T", t0.Add(2*time.Hour)); st.Priority != tc.want {
				t.Fatalf("priority %q, want %q", st.Priority, tc.want)
			}
		})
	}
}

func TestKnownClient_ExcludesOwnDomains(t *testing.T) {
	for _, tc := range []struct {
		name, from string
		topClient  bool
		want       bool
	}{
		{"colleague", "gm@example.com", false, false},
		{"case insensitive", "GM@EXAMPLE.COM", false, false},
		{"disabled mailbox domain", "colleague@other.example", false, false},
		{"subdomain", "gm@team.example.com", false, false},
		{"disabled mailbox subdomain", "colleague@team.other.example", false, false},
		{"own domain in top clients", "gm@example.com", true, false},
		{"own subdomain in top clients", "gm@team.example.com", true, false},
		{"external", "a@external.example", false, true},
		{"near-miss domain", "a@notexample.com", false, true},
		{"domain suffix", "a@example.com.example", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.Mailboxes = append(e.cfg.Mailboxes, config.MailboxConfig{Address: "owner@OTHER.EXAMPLE", Enabled: false})
			if tc.topClient {
				e.cfg.TopClients = append(e.cfg.TopClients, "example.com")
			}
			e.add(t, mopt{thread: "c:history", folder: "sentitems", from: "business@example.com", to: []string{tc.from}, at: t0.Add(-time.Hour)})
			known, err := knownClient(context.Background(), e.s, e.cfg, &store.ThreadMessage{From: tc.from, ReceivedAt: t0})
			if err != nil || known != tc.want {
				t.Fatalf("knownClient(%q) = %v, err=%v; want %v", tc.from, known, err, tc.want)
			}
		})
	}
}

func TestPriority_OutboundHistoryOwnDomainExclusion(t *testing.T) {
	for _, tc := range []struct {
		name, from, want string
	}{
		{"own subdomain", "gm@team.example.com", PriorityP2},
		{"disabled mailbox subdomain", "colleague@team.other.example", PriorityP2},
		{"external domain", "a@external.example", PriorityP0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.Mailboxes = append(e.cfg.Mailboxes, config.MailboxConfig{Address: "owner@OTHER.EXAMPLE", Enabled: false})
			e.add(t, mopt{thread: "c:history", folder: "sentitems", from: "business@example.com", to: []string{tc.from}, at: t0.Add(-time.Hour)})
			e.add(t, mopt{from: tc.from, subject: "Info", label: "clienti", at: t0})
			if st := e.status(t, "c:T", t0.Add(clientSLA-time.Hour)); st.NeedsReply || st.Priority != PriorityP2 {
				t.Fatalf("before SLA breach: %+v", st)
			}
			st := e.status(t, "c:T", t0.Add(clientSLA+time.Hour))
			if !st.NeedsReply || st.Priority != tc.want || !st.DueAt.Equal(t0.Add(thresholds["clienti"])) {
				t.Fatalf("after SLA breach: %+v; want needs reply and priority %q", st, tc.want)
			}
		})
	}
}

func TestPriority_FreeMailClientsRequireExactAddress(t *testing.T) {
	for _, dom := range []string{"gmail.com", "googlemail.com", "outlook.com", "hotmail.com", "hotmail.it", "live.com", "live.it", "yahoo.com", "yahoo.it", "icloud.com", "me.com", "libero.it", "virgilio.it", "tiscali.it", "alice.it", "proton.me", "protonmail.com"} {
		for _, tc := range []struct {
			name      string
			recipient string
			cc        bool
			want      string
		}{
			{"same To", "ALICE@" + strings.ToUpper(dom), false, PriorityP0},
			{"same Cc", "ALICE@" + strings.ToUpper(dom), true, PriorityP0},
			{"other address", "bob@" + dom, false, PriorityP1},
		} {
			t.Run(dom+"/"+tc.name, func(t *testing.T) {
				e := newEnv(t)
				h := mopt{thread: "c:history", folder: "sentitems", from: "business@example.com", at: t0.Add(-time.Hour)}
				if tc.cc {
					h.cc = []string{tc.recipient}
				} else {
					h.to = []string{tc.recipient}
				}
				e.add(t, h)
				e.add(t, mopt{from: "alice@" + dom, subject: "URGENTE", label: "clienti", at: t0})
				if st := e.status(t, "c:T", t0.Add(time.Hour)); st.Priority != tc.want {
					t.Fatalf("priority %q, want %q", st.Priority, tc.want)
				}
			})
		}
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

func TestNeedsReply_InternalOnlyMessageDoesNotClear(t *testing.T) {
	e := newEnv(t)
	e.add(t, mopt{from: "mario@client.example", subject: "Contratto", label: "clienti", at: t0})
	e.add(t, mopt{folder: "sentitems", from: "business@example.com", to: []string{"boss@example.com"}, subject: "Re: Contratto", label: "clienti", at: t0.Add(time.Hour)})
	e.add(t, mopt{from: "collega@example.com", to: []string{"business@example.com"}, subject: "Re: Contratto", label: "clienti", at: t0.Add(2 * time.Hour)})
	if st := e.status(t, "c:T", t0.Add(30*time.Hour)); !st.NeedsReply {
		t.Fatal("internal-only discussion cleared a client thread nobody answered")
	}
}

func TestPriority_KnownByEarlierMessageInSameThread(t *testing.T) {
	e := newEnv(t)
	e.add(t, mopt{from: "a@newclient.example", subject: "Progetto", label: "clienti", at: t0.Add(-48 * time.Hour)})
	e.add(t, mopt{folder: "sentitems", from: "business@example.com", to: []string{"a@newclient.example"}, subject: "Re: Progetto", label: "clienti", at: t0.Add(-47 * time.Hour)})
	e.add(t, mopt{from: "a@newclient.example", subject: "Re: Progetto", body: "Serve entro oggi", label: "clienti", at: t0})
	if st := e.status(t, "c:T", t0.Add(time.Hour)); st.Priority != PriorityP0 {
		t.Fatalf("earlier outbound in the same thread makes the domain known: priority %q", st.Priority)
	}
}

func TestRefreshHandler_NoAlertForDisabledMailbox(t *testing.T) {
	e := newEnv(t)
	e.cfg.Mailboxes[0].Enabled = false // business@: digest:true but disabled
	id := e.add(t, mopt{from: "a@bigbrand.example", subject: "URGENTE", label: "clienti", at: t0})
	alerts := 0
	h := RefreshHandler(e.s, e.cfg, func(context.Context, []store.ThreadRow) error { alerts++; return nil },
		func() time.Time { return t0.Add(time.Hour) })
	if err := h(context.Background(), &store.Job{Ref: id}); err != nil {
		t.Fatal(err)
	}
	if alerts != 0 {
		t.Fatal("P0 alert for a disabled mailbox")
	}
}

func TestRefreshHandler_ConcurrentAlertsOnce(t *testing.T) {
	e := newEnv(t)
	id1 := e.add(t, mopt{from: "a@bigbrand.example", subject: "URGENTE consegna", label: "clienti", at: t0})
	id2 := e.add(t, mopt{from: "a@bigbrand.example", subject: "Re: URGENTE consegna", label: "clienti", at: t0.Add(time.Minute)})
	var mu sync.Mutex
	alerts := 0
	alert := func(context.Context, []store.ThreadRow) error {
		time.Sleep(30 * time.Millisecond) // widen the race window
		mu.Lock()
		alerts++
		mu.Unlock()
		return nil
	}
	h := RefreshHandler(e.s, e.cfg, alert, func() time.Time { return t0.Add(time.Hour) })
	var wg sync.WaitGroup
	for _, id := range []int64{id1, id2, id1, id2} {
		wg.Go(func() {
			if err := h(context.Background(), &store.Job{Ref: id}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if alerts != 1 {
		t.Fatalf("concurrent refreshes sent %d alerts, want 1", alerts)
	}
}

func TestRecompute_ConcurrentSnapshotsNeverWriteStale(t *testing.T) {
	e := newEnv(t)
	e.add(t, mopt{from: "mario@client.example", subject: "Contratto", label: "clienti", at: t0})
	e.add(t, mopt{folder: "sentitems", from: "business@example.com", to: []string{"mario@client.example"}, subject: "Re", label: "clienti", at: t0.Add(time.Hour)})
	now := t0.Add(30 * time.Hour)
	done := make(chan struct{})
	beforeSave = func() { // runs once, between the first recompute's read and write
		beforeSave = nil
		e.add(t, mopt{from: "mario@client.example", subject: "Re: Re", label: "clienti", at: t0.Add(2 * time.Hour)})
		go func() {
			defer close(done)
			if _, err := Recompute(context.Background(), e.s, "c:T", e.cfg, now); err != nil {
				t.Error(err)
			}
		}()
		time.Sleep(50 * time.Millisecond)
	}
	defer func() { beforeSave = nil }()
	if _, err := Recompute(context.Background(), e.s, "c:T", e.cfg, now); err != nil {
		t.Fatal(err)
	}
	<-done
	row, _ := e.s.Thread(context.Background(), "c:T")
	if !row.NeedsReply || row.LastInboundAt == nil || !row.LastInboundAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("stale snapshot overwrote the newer state: %+v", row)
	}
}

func TestRefreshHandler_UnknownDeliveryKeepsDayClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		calls int
	}{
		{"no answer: may be delivered", fmt.Errorf("alert: send: %w", graph.ErrDeliveryUnknown), 1},
		{"rejected: retried", fmt.Errorf("alert: send: %w", &graph.HTTPError{Status: 503}), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.add(t, mopt{from: "a@bigbrand.example", subject: "URGENTE consegna", label: "clienti", at: t0})
			calls := 0
			alert := func(context.Context, []store.ThreadRow) error { calls++; return tc.err }
			h := RefreshHandler(e.s, e.cfg, alert, func() time.Time { return t0.Add(time.Hour) })
			for range 2 {
				_ = h(context.Background(), &store.Job{Ref: id})
			}
			if calls != tc.calls {
				t.Fatalf("alert attempts = %d, want %d", calls, tc.calls)
			}
		})
	}
}
