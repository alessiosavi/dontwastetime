package classify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/graph"
)

func taxonomy(t *testing.T) *Taxonomy {
	t.Helper()
	tx, err := LoadTaxonomy("../../taxonomy.json")
	if err != nil {
		t.Fatalf("LoadTaxonomy: %v", err)
	}
	return tx
}

func inbound(from, subject string) graph.Message {
	return graph.Message{ID: "g1", Mailbox: "admin@example.com", Folder: "inbox", From: from, Subject: subject,
		ReceivedAt: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)}
}

func TestLoadTaxonomy_RepoFile(t *testing.T) {
	tx := taxonomy(t)
	if tx.Version != "v2" || len(tx.Labels) != 18 || len(tx.Order) != 18 {
		t.Fatalf("version %q labels %d order %d", tx.Version, len(tx.Labels), len(tx.Order))
	}
	if tx.Labels["clienti"].Title != "Clienti" || !tx.Has("admin.invoices") || tx.Has("nope") {
		t.Fatalf("labels: %+v", tx.Labels["clienti"])
	}
}

func TestLoadTaxonomy_RequiresUnsure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.json")
	_ = os.WriteFile(p, []byte(`{"version":"x","labels":[{"key":"clienti","title":"Clienti"}]}`), 0o600)
	if _, err := LoadTaxonomy(p); err == nil || !strings.Contains(err.Error(), "unsure") {
		t.Fatalf("want error naming unsure, got %v", err)
	}
	_ = os.WriteFile(p, []byte(`{"version":"x","labels":[{"key":"unsure"},{"key":"unsure"}]}`), 0o600)
	if _, err := LoadTaxonomy(p); err == nil {
		t.Fatal("duplicate label accepted")
	}
}

func TestRules_InvoicesByAttachment(t *testing.T) {
	tx := taxonomy(t)
	cases := []struct {
		name, subject string
		atts          []graph.Attachment
		entity        string
	}{
		{"pdf+srl", "Fattura n. 123 - Example S.r.l.", []graph.Attachment{{Name: "fattura_123.pdf", ContentType: "application/pdf"}}, "SRL"},
		{"xml+ltd", "Invoice 2026/44 Example Ltd", []graph.Attachment{{Name: "IT0123_44.xml", ContentType: "text/xml"}}, "LTD"},
		{"credit note branch", "Nota di credito", []graph.Attachment{{Name: "nc_branch.pdf", ContentType: "application/pdf"}}, "Branch"},
		{"no entity", "Fattura Aruba", []graph.Attachment{{Name: "a.pdf", ContentType: "application/pdf"}}, ""},
	}
	for _, c := range cases {
		m := inbound("fatture@vendor.example", c.subject)
		m.HasAttachments = true
		r, ok := Rules(m, nil, c.atts, tx)
		if !ok || r.Label != "admin.invoices" || r.Entity != c.entity || r.Method != MethodRule || r.Confidence < 0.85 {
			t.Errorf("%s: got %+v ok=%v", c.name, r, ok)
		}
	}
	// Invoice words without a pdf/xml attachment are not enough for a rule.
	if r, ok := Rules(inbound("x@vendor.example", "Fattura in arrivo"), nil, nil, tx); ok {
		t.Errorf("invoice without attachment matched: %+v", r)
	}
}

func TestRules_NewsletterByHeader(t *testing.T) {
	tx := taxonomy(t)
	for _, h := range []graph.Header{{Name: "List-Id", Value: "<news.example>"}, {Name: "Precedence", Value: "bulk"}, {Name: "Precedence", Value: "list"}} {
		r, ok := Rules(inbound("news@brand.example", "Le novità di novembre"), []graph.Header{h}, nil, tx)
		if !ok || r.Label != "marketing.news" {
			t.Errorf("header %s: got %+v ok=%v", h.Name, r, ok)
		}
	}
}

func TestRules_AutoByHeader(t *testing.T) {
	tx := taxonomy(t)
	r, ok := Rules(inbound("mario@client.example", "Re: Rinnovo"), []graph.Header{{Name: "Auto-Submitted", Value: "auto-replied"}}, nil, tx)
	if !ok || r.Label != "notifications" {
		t.Fatalf("auto-reply: %+v ok=%v", r, ok)
	}
	if _, ok := Rules(inbound("mario@client.example", "Re: Rinnovo"), []graph.Header{{Name: "Auto-Submitted", Value: "no"}}, nil, tx); ok {
		t.Fatal("Auto-Submitted: no is a human message")
	}
}

func TestRules_NotificationsBySender(t *testing.T) {
	tx := taxonomy(t)
	cases := map[string]string{
		"no-reply@shop.example":       "Conferma del tuo ordine",
		"noreply@bank.example":        "Notifica di accesso",
		"mailer-daemon@example.com":     "Undeliverable: Rinnovo",
		"posta-certificata@pec.aruba": "ACCETTAZIONE: Invio documenti",
		"mario@client.example":        "Risposta automatica: fuori ufficio",
	}
	for from, subj := range cases {
		if r, ok := Rules(inbound(from, subj), nil, nil, tx); !ok || r.Label != "notifications" {
			t.Errorf("%s / %q: got %+v ok=%v", from, subj, r, ok)
		}
	}
}

func TestRules_UnsubscribeIsOnlyAHint(t *testing.T) {
	tx := taxonomy(t)
	for _, subject := range []string{"Monthly product report", "Payment failed", "Security alert", "Newsletter"} {
		r, ok := Rules(inbound("mail@vendor.example", subject),
			[]graph.Header{{Name: "lIsT-uNsUbScRiBe", Value: "<mailto:u@x>"}}, nil, tx)
		if ok {
			t.Errorf("%q classified from unsubscribe alone: %+v", subject, r)
		}
	}
}

func TestRules_NoReplyPaymentSplit(t *testing.T) {
	tx := taxonomy(t)
	for _, word := range []string{"pagamento", "payment", "ricevuta", "receipt", "addebito", "charge", "fattura", "invoice"} {
		t.Run(word, func(t *testing.T) {
			r, ok := Rules(inbound("no-reply@vendor.example", "Conferma "+word), nil, nil, tx)
			if !ok || r.Label != "admin.invoices" || r.Confidence != 0.85 || r.Rationale != "mittente no-reply e oggetto da pagamento" {
				t.Fatalf("payment rule: %+v, ok=%v", r, ok)
			}
		})
	}
	for _, word := range []string{"conferma", "confirmation", "notifica", "notification", "ordine", "order"} {
		if r, ok := Rules(inbound("noreply@vendor.example", word), nil, nil, tx); !ok || r.Label != "notifications" {
			t.Errorf("%s: %+v, ok=%v", word, r, ok)
		}
	}
	if r, ok := Rules(inbound("human@vendor.example", "Payment"), nil, nil, tx); ok {
		t.Fatalf("payment without no-reply sender: %+v", r)
	}
}

func TestRules_TechAlertsBySender(t *testing.T) {
	tx := taxonomy(t)
	if r, ok := Rules(inbound("notifications@github.com", "[repo] CI failed"), nil, nil, tx); !ok || r.Label != "tech.alerts" {
		t.Fatalf("github notification: %+v ok=%v", r, ok)
	}
}

func TestRules_NotificationDomains(t *testing.T) {
	tx := taxonomy(t)
	for _, dom := range []string{"cal.com", "cal.eu", "calendly.com", "tito.io", "eventbrite.com", "eventbrite.it", "trenitalia.it"} {
		for _, from := range []string{"hello@" + dom, "hello@mail." + dom, "HELLO@" + strings.ToUpper(dom)} {
			t.Run(from, func(t *testing.T) {
				r, ok := Rules(inbound(from, "Product demo between Operator and Mario"), nil, nil, tx)
				if !ok || r.Label != "notifications" || r.Confidence != 0.9 || r.Method != MethodRule ||
					r.Rationale != "mittente di notifiche automatiche "+dom {
					t.Fatalf("notification sender: %+v ok=%v", r, ok)
				}
			})
		}
	}
	for _, from := range []string{"hello@notcal.com", "hello@cal.com.example", "hello@nottito.io"} {
		t.Run(from, func(t *testing.T) {
			if r, ok := Rules(inbound(from, "Product demo"), nil, nil, tx); ok {
				t.Fatalf("near-miss sender matched: %+v", r)
			}
		})
	}
	// Sender rules precede bulk/list headers.
	if r, ok := Rules(inbound("hello@cal.com", "Product demo"), []graph.Header{{Name: "List-Id", Value: "bookings"}}, nil, tx); !ok || r.Label != "notifications" {
		t.Fatalf("notification with list header: %+v ok=%v", r, ok)
	}
}

func TestRules_AccountingKeywords(t *testing.T) {
	tx := taxonomy(t)
	for _, subj := range []string{"F24 da pagare entro il 16", "Registro IVA ottobre", "Chiusura mensile ottobre"} {
		if r, ok := Rules(inbound("studio@commercialista.example", subj), nil, nil, tx); !ok || r.Label != "accounting" {
			t.Errorf("%q: got %+v ok=%v", subj, r, ok)
		}
	}
	// Internal sender (same domain as the mailbox) is not "obvious accounting".
	if r, ok := Rules(inbound("person.two@example.com", "IVA ottobre"), nil, nil, tx); ok {
		t.Errorf("internal sender matched accounting: %+v", r)
	}
	// "iva" inside another word must not match.
	if r, ok := Rules(inbound("x@client.example", "Arriva la proposta"), nil, nil, tx); ok {
		t.Errorf("substring matched: %+v", r)
	}
}

func TestRules_NoMatchReturnsFalse(t *testing.T) {
	tx := taxonomy(t)
	if r, ok := Rules(inbound("mario@client.example", "Rinnovo contratto 2027"), nil, nil, tx); ok {
		t.Fatalf("plain client mail matched a rule: %+v", r)
	}
}

func TestRules_InvoiceExceptionsDeferToLLM(t *testing.T) {
	tx := taxonomy(t)
	pdf := []graph.Attachment{{Name: "f.pdf", ContentType: "application/pdf"}}
	for _, subj := range []string{"Fattura collaboratore Collaborator ottobre", "Fattura prestazione occasionale", "Fattura location evento 12/11", "Fattura catering evento"} {
		m := inbound("x@vendor.example", subj)
		m.HasAttachments = true
		if r, ok := Rules(m, nil, pdf, tx); ok {
			t.Errorf("%q: collaborator/venue invoices follow their own cassetto (LLM), got rule %+v", subj, r)
		}
	}
}
