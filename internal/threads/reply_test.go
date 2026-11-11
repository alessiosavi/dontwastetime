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
