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
