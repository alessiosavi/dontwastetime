package classify

import (
	"path"
	"regexp"
	"strings"

	"github.com/alessiosavi/GoGPUtils/stringutil"
	"github.com/alessiosavi/GoGPUtils/textnorm"

	"github.com/alessiosavi/go-dontwastetime/internal/graph"
)

// Result is one classification answer (rules or LLM).
type Result struct {
	ID               int64 // message row id (LLM batches)
	Label, Entity    string
	Confidence       float64
	Urgent, Negative bool
	Method           string
	Rationale        string
}

var (
	searchNorm = textnorm.SearchPreset()

	noReplyLocal = []string{"no-reply", "noreply", "no_reply", "do-not-reply", "donotreply", "notification", "notifiche", "notify"}
	bounceLocal  = []string{"mailer-daemon", "postmaster"}
	receiptWords = []string{"ricevuta", "receipt", "conferma", "confirmation", "notifica", "notification", "ordine", "order", "pagamento", "payment"}
	pecWords     = []string{"accettazione", "avvenuta consegna", "mancata consegna", "posta certificata", "anomalia messaggio"}
	oofWords     = []string{"risposta automatica", "automatic reply", "auto reply", "autoreply", "out of office", "fuori ufficio", "fuori sede", "undeliverable", "non recapitabile", "delivery status notification", "mail delivery failed"}
	// Collaborator and event-venue invoices follow their own cassetto
	// (personale.collaborators, eventi.post): never ruled as admin.invoices.
	invoiceExceptions = []string{"prestazione", "occasionale", "evento", "eventi", "location", "venue", "catering", "allestimento"}
	invoiceWords      = []string{"fattura", "fatture", "invoice", "nota di credito", "credit note", "fattura elettronica"}
	accountWords      = []string{"f24", "iva", "registro iva", "liquidazione iva", "chiusura", "chiusura mensile", "bilancio", "ravvedimento", "cassetto fiscale", "dichiarazione dei redditi"}
	techDomains       = []string{"github.com", "sentry.io", "vercel.com", "amazonaws.com", "aws.amazon.com", "statuspage.io", "pagerduty.com", "uptimerobot.com", "datadoghq.com", "cloudflare.com", "supabase.io", "supabase.com"}
	entitySRL         = regexp.MustCompile(`(?i)example[\W_]*s\.?\s?r\.?\s?l`)
	entityLTD         = regexp.MustCompile(`(?i)example[\W_]*(ltd|limited)`)
	entityBranch     = regexp.MustCompile(`(?i)branch[\W_]*one`)
)

// norm is the match form of a subject: lower-case letters/digits/spaces,
// padded so " word " checks are whole-word.
func norm(s string) string {
	n, err := searchNorm.Run(s)
	if err != nil {
		n = strings.ToLower(s)
	}
	return " " + n + " "
}

func hasWord(normalized string, words []string) bool {
	for _, w := range words {
		if strings.Contains(normalized, " "+w+" ") {
			return true
		}
	}
	return false
}

func header(hs []graph.Header, name string) (string, bool) {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return strings.TrimSpace(h.Value), true
		}
	}
	return "", false
}

func domainOf(addr string) string {
	_, d, _ := strings.Cut(strings.ToLower(addr), "@")
	return d
}

func localOf(addr string) string {
	l, _, _ := strings.Cut(strings.ToLower(addr), "@")
	return l
}

func rule(label string, conf float64, why string) (Result, bool) {
	return Result{Label: label, Confidence: conf, Method: MethodRule, Rationale: why}, true
}

// Rules applies the deterministic families in order: automatic/bounce/PEC
// receipts, invoice attachments, tech senders, bulk/list headers, no-reply
// receipts, accounting keywords. ok=false means "ask the LLM".
func Rules(m graph.Message, headers []graph.Header, atts []graph.Attachment, t *Taxonomy) (Result, bool) {
	subj := norm(m.Subject)
	local, dom := localOf(m.From), domainOf(m.From)

	// Automatic replies, bounces, PEC receipts → notifications.
	if v, ok := header(headers, "Auto-Submitted"); ok && !strings.EqualFold(v, "no") {
		return rule("notifications", 0.95, "intestazione Auto-Submitted")
	}
	if _, ok := header(headers, "X-Autoreply"); ok {
		return rule("notifications", 0.95, "intestazione X-Autoreply")
	}
	if v, _ := header(headers, "Precedence"); strings.EqualFold(v, "auto_reply") {
		return rule("notifications", 0.95, "risposta automatica (Precedence)")
	}
	if stringutil.HasAnyPrefix(local, bounceLocal...) {
		return rule("notifications", 0.95, "mittente di mancato recapito")
	}
	if hasWord(subj, pecWords) || hasWord(subj, oofWords) {
		return rule("notifications", 0.9, "oggetto da ricevuta o risposta automatica")
	}

	// Invoices: a pdf/xml attachment plus invoice words.
	if hasInvoiceAttachment(atts) && hasWord(subj, invoiceWords) &&
		!hasWord(subj, invoiceExceptions) && !strings.Contains(subj, " collaborat") {
		r, _ := rule("admin.invoices", 0.9, "allegato pdf/xml e oggetto da fattura")
		r.Entity = entity(m.Subject, atts)
		return r, true
	}

	// Tech notifications by sender domain.
	for _, d := range techDomains {
		if dom == d || strings.HasSuffix(dom, "."+d) {
			return rule("tech.alerts", 0.9, "mittente tecnico "+d)
		}
	}

	// Bulk / mailing lists → newsletter.
	if _, ok := header(headers, "List-Unsubscribe"); ok {
		return rule("marketing.news", 0.9, "newsletter (List-Unsubscribe)")
	}
	if _, ok := header(headers, "List-Id"); ok {
		return rule("marketing.news", 0.9, "mailing list (List-Id)")
	}
	if v, _ := header(headers, "Precedence"); strings.EqualFold(v, "bulk") || strings.EqualFold(v, "list") {
		return rule("marketing.news", 0.85, "invio massivo (Precedence: "+v+")")
	}

	// No-reply sender with a receipt-ish subject → notifications.
	if stringutil.HasAnyPrefix(local, noReplyLocal...) && hasWord(subj, receiptWords) {
		return rule("notifications", 0.9, "mittente no-reply e oggetto da ricevuta")
	}

	// Accounting keywords from an external sender.
	if dom != "" && dom != domainOf(m.Mailbox) && hasWord(subj, accountWords) {
		return rule("accounting", 0.85, "parola chiave contabile da mittente esterno")
	}
	return Result{}, false
}

func hasInvoiceAttachment(atts []graph.Attachment) bool {
	for _, a := range atts {
		ext := strings.ToLower(path.Ext(a.Name))
		ct := strings.ToLower(a.ContentType)
		if ext == ".pdf" || ext == ".xml" || ext == ".p7m" || ct == "application/pdf" || strings.HasSuffix(ct, "/xml") {
			return true
		}
	}
	return false
}

// entity detects which of our legal entities an invoice is addressed to.
func entity(subject string, atts []graph.Attachment) string {
	texts := []string{subject}
	for _, a := range atts {
		texts = append(texts, a.Name)
	}
	for _, s := range texts {
		switch {
		case entityBranch.MatchString(s):
			return "Branch"
		case entityLTD.MatchString(s):
			return "LTD"
		case entitySRL.MatchString(s):
			return "SRL"
		}
	}
	return ""
}

// Flags derives the stored message flags from internet headers: auto
// (auto-generated / auto-replied), bulk (Precedence bulk|junk|list) and
// list (mailing-list headers).
func Flags(headers []graph.Header) (auto, bulk, list bool) {
	if v, ok := header(headers, "Auto-Submitted"); ok && !strings.EqualFold(v, "no") {
		auto = true
	}
	if _, ok := header(headers, "X-Autoreply"); ok {
		auto = true
	}
	switch v, _ := header(headers, "Precedence"); strings.ToLower(v) {
	case "auto_reply":
		auto = true
	case "bulk", "junk", "list":
		bulk = true
	}
	_, unsub := header(headers, "List-Unsubscribe")
	_, listID := header(headers, "List-Id")
	return auto, bulk, unsub || listID
}
