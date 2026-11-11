package threads

import (
	"context"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

func TestReplyDecision_MatchesUnansweredRecompute(t *testing.T) {
	for _, label := range []string{"clienti", "business.dev", "ops.spaces", "eventi.internal", "eventi.external", "eventi.post", "admin.requests", "admin.invoices", "accounting", "hr.recruiting", "notifications", "marketing.news", "unsure"} {
		for _, suppressed := range []bool{false, true} {
			t.Run(label+"/"+map[bool]string{true: "auto", false: "human"}[suppressed], func(t *testing.T) {
				e := newEnv(t)
				e.add(t, mopt{from: "a@unknown.example", subject: "Info", label: label, auto: suppressed, at: t0})
				msgs, err := e.s.ThreadMessages(context.Background(), "c:T")
				if err != nil {
					t.Fatal(err)
				}
				got := ReplyDecision(label, &msgs[0])
				st := e.status(t, "c:T", t0.Add(100*time.Hour))
				if got != st.NeedsReply {
					t.Fatalf("pure=%v, Recompute=%+v", got, st)
				}
			})
		}
	}
}

func TestReplyDecision_SuppressionAndNoTiming(t *testing.T) {
	for _, m := range []store.ThreadMessage{
		{From: "noreply@x"}, {From: "postmaster@x"}, {Subject: "ACCETTAZIONE: Contratto"},
		{Subject: "Risposta automatica: Contratto"}, {Subject: "Accettato: Riunione"},
		{IsAuto: true}, {IsBulk: true}, {IsList: true},
	} {
		m.Urgent, m.Negative, m.ReplyExpected = true, true, new(false)
		if ReplyDecision("clienti", &m) {
			t.Errorf("suppressed message opened a reply obligation: %+v", m)
		}
	}
	if !ReplyDecision("clienti", &store.ThreadMessage{From: "a@client.example", ReceivedAt: t0, InsertedAt: t0.Add(60 * 24 * time.Hour)}) {
		t.Fatal("pure decision applied age/backlog")
	}
	if ReplyDecision("clienti", nil) {
		t.Fatal("nil inbound opened a reply obligation")
	}
}

func TestReplyDecision_ExplicitAndFallback(t *testing.T) {
	for _, label := range []string{"clienti", "admin.requests", "accounting", "hr.recruiting", "unsure"} {
		for _, reply := range []*bool{nil, new(true), new(false)} {
			m := store.ThreadMessage{From: "person@external.example", ReplyExpected: reply}
			_, tracked := thresholds[label]
			want := tracked
			if reply != nil {
				want = *reply
			}
			if got := ReplyDecision(label, &m); got != want {
				t.Errorf("%s reply=%v: got %v, want %v", label, reply, got, want)
			}
			for _, suppressed := range []store.ThreadMessage{
				{IsAuto: true}, {IsBulk: true}, {IsList: true},
				{From: "no-reply@x"}, {Subject: "ACCETTAZIONE: Test"},
				{Subject: "Accettato: Meeting"}, {Subject: "Out of office"},
			} {
				suppressed.ReplyExpected = new(true)
				if ReplyDecision(label, &suppressed) {
					t.Errorf("explicit true bypassed suppression: %+v", suppressed)
				}
			}
		}
	}
}

func TestReplyDecision_NeverReplyLabels(t *testing.T) {
	for _, label := range []string{"personal", "notifications", "marketing.news", "tech.alerts"} {
		t.Run(label, func(t *testing.T) {
			for _, reply := range []*bool{nil, new(true), new(false)} {
				m := store.ThreadMessage{From: "person@external.example", Urgent: true, Negative: true, ReplyExpected: reply}
				if ReplyDecision(label, &m) {
					t.Errorf("excluded label opened an obligation with reply_expected=%v", reply)
				}
			}
		})
	}
}

func TestRefreshHandler_ClientiReplyExpectedFalse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		msg    mopt
		wantP0 bool
	}{
		{"urgent subject", mopt{label: "clienti", subject: "URGENTE consegna"}, true},
		{"urgent preview", mopt{label: "clienti", subject: "Consegna", body: "Serve entro domani"}, true},
		{"urgent flag", mopt{label: "clienti", subject: "Consegna", urgent: true}, true},
		{"negative flag", mopt{label: "clienti", subject: "Consegna", negative: true}, true},
		{"thanks received", mopt{label: "clienti", subject: "Grazie, ricevuto"}, false},
		{"suppressed", mopt{label: "clienti", subject: "URGENTE consegna", negative: true, auto: true}, false},
		{"personal", mopt{label: "personal", subject: "URGENTE consegna", negative: true}, false},
		{"other label", mopt{label: "business.dev", subject: "URGENTE consegna", negative: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			o := tc.msg
			o.from, o.at, o.replyExpected = "a@bigbrand.example", t0, new(false)
			id := e.add(t, o)
			alerts := 0
			h := RefreshHandler(e.s, e.cfg, func(_ context.Context, rows []store.ThreadRow) error {
				alerts += len(rows)
				for _, row := range rows {
					if row.ThreadKey != "c:T" || !row.NeedsReply || row.Priority != PriorityP0 {
						t.Errorf("unexpected alert row: %+v", row)
					}
				}
				return nil
			}, func() time.Time { return t0.Add(time.Hour) })
			for range 2 {
				if err := h(ctx, &store.Job{Ref: id}); err != nil {
					t.Fatal(err)
				}
			}
			row, err := e.s.Thread(ctx, "c:T")
			if err != nil {
				t.Fatal(err)
			}
			wantPriority, wantAlerts := "", 0
			if tc.wantP0 {
				wantPriority, wantAlerts = PriorityP0, 1
				if row.ReplyDueAt == nil || !row.ReplyDueAt.Equal(t0.Add(24*time.Hour)) {
					t.Errorf("reply due at %v, want %v", row.ReplyDueAt, t0.Add(24*time.Hour))
				}
			} else {
				if row.ReplyDueAt != nil {
					t.Errorf("unexpected reply obligation due at %v", row.ReplyDueAt)
				}
				if st := e.status(t, "c:T", t0.Add(25*time.Hour)); st.NeedsReply || !st.DueAt.IsZero() || st.Priority != "" {
					t.Errorf("unexpected obligation after client SLA: %+v", st)
				}
			}
			if row.NeedsReply != tc.wantP0 || row.Priority != wantPriority {
				t.Errorf("needs reply=%v, priority=%q; want %v, %q", row.NeedsReply, row.Priority, tc.wantP0, wantPriority)
			}
			if alerts != wantAlerts {
				t.Errorf("alerts on the same day = %d, want %d", alerts, wantAlerts)
			}
		})
	}
}

func TestRecompute_NeverReplyLabels(t *testing.T) {
	for _, label := range []string{"personal", "notifications", "marketing.news", "tech.alerts"} {
		for _, existing := range []bool{false, true} {
			t.Run(label+map[bool]string{false: "/fresh", true: "/existing"}[existing], func(t *testing.T) {
				e := newEnv(t)
				ctx := context.Background()
				e.add(t, mopt{from: "person@external.example", subject: "Info", label: label, at: t0, replyExpected: new(true)})
				if existing {
					due := t0.Add(24 * time.Hour)
					if err := e.s.SaveThreadState(ctx, store.ThreadRow{
						ThreadKey: "c:T", Mailbox: "business@example.com", Cassetto: label,
						LastInboundAt: &t0, NeedsReply: true, ReplyDueAt: &due, Priority: PriorityP0,
					}, t0); err != nil {
						t.Fatal(err)
					}
				}
				st := e.status(t, "c:T", t0.Add(100*time.Hour))
				if st.NeedsReply || !st.DueAt.IsZero() || st.Priority != "" {
					t.Errorf("excluded label has an obligation: %+v", st)
				}
				row, err := e.s.Thread(ctx, "c:T")
				if err != nil {
					t.Fatal(err)
				}
				if row.NeedsReply || row.ReplyDueAt != nil || row.Priority != "" {
					t.Errorf("persisted obligation was not cleared: %+v", row)
				}
				rows, err := e.s.ListNeedsReply(ctx)
				if err != nil || len(rows) != 0 {
					t.Errorf("needs-reply rows=%d, err=%v; want none", len(rows), err)
				}
			})
		}
	}
}

func TestRecompute_ReplyExpectedSLA(t *testing.T) {
	for _, tc := range []struct {
		label string
		reply *bool
		sla   time.Duration
	}{
		{"clienti", new(true), 24 * time.Hour},
		{"accounting", new(true), 48 * time.Hour},
		{"hr.recruiting", new(true), 48 * time.Hour},
		{"admin.requests", nil, 48 * time.Hour},
		{"clienti", new(false), 0},
		{"accounting", nil, 0},
	} {
		t.Run(tc.label+map[bool]string{true: "/explicit", false: "/fallback"}[tc.reply != nil], func(t *testing.T) {
			e := newEnv(t)
			e.add(t, mopt{from: "person@unknown.example", subject: "Info", label: tc.label, at: t0, replyExpected: tc.reply})
			st := e.status(t, "c:T", t0.Add(47*time.Hour))
			if tc.sla == 0 {
				if st.NeedsReply || !st.DueAt.IsZero() {
					t.Fatalf("unexpected obligation: %+v", st)
				}
				return
			}
			if !st.DueAt.Equal(t0.Add(tc.sla)) || st.NeedsReply != (47*time.Hour >= tc.sla) {
				t.Fatalf("before/default SLA: %+v, sla=%v", st, tc.sla)
			}
			if st = e.status(t, "c:T", t0.Add(tc.sla)); !st.NeedsReply {
				t.Fatalf("SLA elapsed: %+v", st)
			}
			// A new inbound can close the conversation without an outbound.
			e.add(t, mopt{from: "person@unknown.example", subject: "Grazie, risolto", label: tc.label, at: t0.Add(time.Hour), replyExpected: new(false)})
			if st = e.status(t, "c:T", t0.Add(100*time.Hour)); st.NeedsReply || !st.DueAt.IsZero() {
				t.Fatalf("closing inbound reopened obligation: %+v", st)
			}
		})
	}
}
