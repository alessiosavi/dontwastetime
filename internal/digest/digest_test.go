package digest

import (
	"context"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

var update = flag.Bool("update", false, "rewrite golden files")

// at is Tuesday 10 Nov 2026, 08:00 Europe/Rome (07:00 UTC).
var at = time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)

type fx struct {
	s   *store.Store
	cfg *config.Config
	tx  *classify.Taxonomy
	n   int
}

func newFx(t *testing.T) *fx {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "dwt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	tx, err := classify.LoadTaxonomy("../../taxonomy.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Mailboxes: []config.MailboxConfig{
			{Address: "business@example.com", Enabled: true, Digest: true, PollSeconds: 180},
			{Address: "legal@example.com", Enabled: true, Digest: true, PollSeconds: 180},
			{Address: "employee@example.com", Enabled: true, Digest: false, PollSeconds: 180},
		},
		DigestRecipients: []string{"founder@example.com"}, AlertRecipients: []string{"founder@example.com"},
		DigestSender: "admin@example.com", TopClients: []string{"bigbrand.example"},
		StateDir: t.TempDir(), LLM: config.LLMConfig{DailyMaxTokens: 1000},
	}
	for _, mb := range cfg.Mailboxes {
		_ = s.UpsertMailbox(context.Background(), mb)
	}
	return &fx{s: s, cfg: cfg, tx: tx}
}

func (f *fx) add(t *testing.T, mailbox, from, subject, label string, age time.Duration) int64 {
	t.Helper()
	f.n++
	when := at.Add(-age)
	m := graph.Message{ID: "g" + strings.Repeat("x", f.n), InternetMessageID: "<" + strings.Repeat("x", f.n) + "@t>",
		Mailbox: mailbox, Folder: "inbox", From: from, FromName: "", Subject: subject, BodyText: "testo", BodyPreview: "testo", ReceivedAt: when}
	id, _, err := f.s.UpsertMessage(context.Background(), m, "c:"+subject, when)
	if err != nil {
		t.Fatal(err)
	}
	if label != "" {
		_ = f.s.SaveClassification(context.Background(), store.Classification{MessageID: id, Label: label, Confidence: 0.9,
			Method: "llm", Rationale: "motivo " + label}, when)
		if _, err := threads.Recompute(context.Background(), f.s, "c:"+subject, f.cfg, at); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// seed builds the shared fixture used by the golden and content tests.
func seed(t *testing.T) *fx {
	f := newFx(t)
	f.add(t, "business@example.com", "ceo@bigbrand.example", "Rinnovo contratto 2027", "clienti", 26*time.Hour)
	f.add(t, "business@example.com", "info@prospect.example", "Proposta di collaborazione", "business.dev", 3*time.Hour)
	f.add(t, "legal@example.com", "studio@commercialista.example", "F24 novembre", "accounting", 5*time.Hour)
	f.add(t, "business@example.com", "fatture@vendor.example", "Fattura n. 12", "admin.invoices", 2*time.Hour)
	for i := range 3 {
		f.add(t, "business@example.com", "no-reply@shop.example", "Ricevuta ordine "+string(rune('A'+i)), "notifications", time.Hour)
	}
	f.add(t, "business@example.com", "mamma@family.example", "Cena domenica", "personal", time.Hour)
	f.add(t, "business@example.com", "x@strano.example", "Oggetto ambiguo", "unsure", 4*time.Hour)
	f.add(t, "employee@example.com", "friend@else.example", "Casella personale dipendente", "clienti", 2*time.Hour)
	f.add(t, "business@example.com", "old@client.example", "Mail di ieri l'altro", "clienti", 30*24*time.Hour) // outside the window
	return f
}

func golden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v (run with -update)", path, err)
	}
	if got != string(want) {
		t.Fatalf("output differs from %s (run with -update and review the diff):\n%s", path, got)
	}
}

func TestBuild_GoldenMarkdown(t *testing.T) {
	f := seed(t)
	d, err := Build(context.Background(), f.s, f.cfg, f.tx, at)
	if err != nil {
		t.Fatal(err)
	}
	if d.Day != "2026-11-10" {
		t.Errorf("day = %s", d.Day)
	}
	golden(t, "testdata/golden.md", d.Markdown)
}

func TestBuild_NotificationsCountOnlyPersonalHidden(t *testing.T) {
	f := seed(t)
	d, err := Build(context.Background(), f.s, f.cfg, f.tx, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"Ricevuta ordine", "Cena domenica", "Casella personale dipendente"} {
		if strings.Contains(d.Markdown, hidden) || strings.Contains(d.HTML, hidden) {
			t.Errorf("digest shows %q", hidden)
		}
	}
	// Old mail is not an arrival, but an unanswered client thread stays pinned in "Serve risposta".
	_, arrivals, _ := strings.Cut(d.Markdown, "## Per cassetto")
	if strings.Contains(arrivals, "Mail di ieri") || !strings.Contains(d.Markdown, "[P1+ · Clienti] old@client.example") {
		t.Error("old client thread: must be pinned in Serve risposta, not listed as an arrival")
	}
	if !strings.Contains(d.Markdown, "**Notifiche:** 3") {
		t.Error("notifications count missing")
	}
	for _, shown := range []string{"Rinnovo contratto 2027", "Proposta di collaborazione", "F24 novembre", "Oggetto ambiguo", "## Da classificare (1)"} {
		if !strings.Contains(d.Markdown, shown) {
			t.Errorf("digest lacks %q", shown)
		}
	}
}

func TestBuild_HealthSection(t *testing.T) {
	f := seed(t)
	ctx := context.Background()
	_ = f.s.SetMailboxState(ctx, "legal@example.com", store.StateDegraded, "403: outside the application access policy?", at)
	_ = f.s.Enqueue(ctx, "classify", 1, at.Add(-45*time.Minute))
	_ = f.s.UsageAdd(ctx, config.Day(at), 1000, 0)
	d, err := Build(ctx, f.s, f.cfg, f.tx, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"legal@example.com", "access policy", "1 in attesa (la più vecchia da 45 min)", "classificatore in pausa: budget giornaliero raggiunto"} {
		if !strings.Contains(d.Markdown, want) {
			t.Errorf("health section lacks %q", want)
		}
	}
	if d.Health.QueuePending != 1 || !d.Health.BudgetReached || len(d.Health.Degraded) != 1 {
		t.Errorf("health = %+v", d.Health)
	}
}

func TestBuild_HTMLEscapes(t *testing.T) {
	f := newFx(t)
	f.add(t, "business@example.com", "evil@x.example", `<script>alert(1)</script>`, "unsure", time.Hour)
	d, err := Build(context.Background(), f.s, f.cfg, f.tx, at)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.HTML, "<script>") || !strings.Contains(d.HTML, "&lt;script&gt;") {
		t.Fatal("subject not escaped in HTML")
	}
}

func TestWindowStart_SkipsWeekend(t *testing.T) {
	mon := time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC) // Mon 08:00 Rome
	if got := WindowStart(mon); !got.Equal(time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("Monday window starts %v, want Friday 08:00 Rome", got)
	}
	if got := WindowStart(at); !got.Equal(at.Add(-24 * time.Hour)) {
		t.Errorf("Tuesday window starts %v", got)
	}
}

func TestSend_ValidatesBeforeNetwork(t *testing.T) {
	f := seed(t)
	srv, c := graphtest.New(t, nil)
	d, _ := Build(context.Background(), f.s, f.cfg, f.tx, at)
	for name, mutate := range map[string]func(*config.Config){
		"bad recipient": func(c *config.Config) { c.DigestRecipients = []string{"not-an-address"} },
		"no recipients": func(c *config.Config) { c.DigestRecipients = nil },
		"banned sender": func(c *config.Config) { c.DigestSender = "gm@example.com" },
	} {
		cfg := *f.cfg
		mutate(&cfg)
		if err := Send(context.Background(), f.s, c, &cfg, d, false, at); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := srv.APICalls.Load(); n != 0 {
		t.Fatalf("invalid send reached the network %d times", n)
	}
}

func TestSend_DryRunWritesNoNetwork(t *testing.T) {
	f := seed(t)
	srv, c := graphtest.New(t, nil)
	d, _ := Build(context.Background(), f.s, f.cfg, f.tx, at)
	if err := Send(context.Background(), f.s, c, f.cfg, d, true, at); err != nil {
		t.Fatal(err)
	}
	if srv.APICalls.Load() != 0 {
		t.Fatal("dry run hit the network")
	}
	for _, ext := range []string{"md", "html"} {
		if _, err := os.Stat(filepath.Join(f.cfg.StateDir, "digests", "2026-11-10."+ext)); err != nil {
			t.Errorf("dry run file %s: %v", ext, err)
		}
	}
	var sent *string
	if err := f.s.DB().QueryRow(`SELECT sent_at FROM digest WHERE date = '2026-11-10'`).Scan(&sent); err != nil || sent != nil {
		t.Fatalf("digest row: sent_at=%v err=%v", sent, err)
	}
}

func TestSend_LiveOncePerDay(t *testing.T) {
	f := seed(t)
	mail, _, c := graphtest.NewMail(t)
	d, _ := Build(context.Background(), f.s, f.cfg, f.tx, at)
	for range 2 {
		if err := Send(context.Background(), f.s, c, f.cfg, d, false, at); err != nil {
			t.Fatal(err)
		}
	}
	if mail.SentCount() != 1 {
		t.Fatalf("sends = %d, want 1 (second call must see sent_at)", mail.SentCount())
	}
	p := mail.Sent[0]
	if p.Sender != "admin@example.com" || p.Payload["saveToSentItems"] != false {
		t.Errorf("send: %+v", p)
	}
	if ok, _ := f.s.DigestSent(context.Background(), "2026-11-10"); !ok {
		t.Error("sent_at not recorded")
	}
}

func TestAlert_P0Only(t *testing.T) {
	f := newFx(t)
	mail, _, c := graphtest.NewMail(t)
	rows := []store.ThreadRow{
		{ThreadKey: "c:a", Mailbox: "business@example.com", Subject: "URGENTE consegna", LastInboundFrom: "ceo@bigbrand.example", Priority: "P0", Cassetto: "clienti"},
		{ThreadKey: "c:b", Mailbox: "business@example.com", Subject: "Info", LastInboundFrom: "x@unknown.example", Priority: "P1", Cassetto: "clienti"},
	}
	if err := Alert(context.Background(), f.s, c, f.cfg, rows, false, at); err != nil {
		t.Fatal(err)
	}
	if mail.SentCount() != 1 {
		t.Fatalf("alert mails = %d", mail.SentCount())
	}
	msg := mail.Sent[0].Payload["message"].(map[string]any)
	body := msg["body"].(map[string]any)["content"].(string)
	if !strings.Contains(msg["subject"].(string), "P0") || !strings.Contains(body, "URGENTE consegna") || strings.Contains(body, "x@unknown.example") {
		t.Fatalf("alert content: subject=%v body=%s", msg["subject"], body)
	}
	var logged int
	_ = f.s.DB().QueryRow(`SELECT count(*) FROM action_log WHERE action = 'p0_alert'`).Scan(&logged)
	if logged != 1 {
		t.Fatalf("alert not logged: %d", logged)
	}
	// Only non-P0 rows: nothing to send.
	if err := Alert(context.Background(), f.s, c, f.cfg, rows[1:], false, at); err != nil || mail.SentCount() != 1 {
		t.Fatalf("non-P0 alert: err=%v sends=%d", err, mail.SentCount())
	}
}

func TestBuild_LinesCarryMessageIDs(t *testing.T) {
	f := seed(t)
	d, err := Build(context.Background(), f.s, f.cfg, f.tx, at)
	if err != nil {
		t.Fatal(err)
	}
	// Message 1 is the unanswered P0 client mail; 2 is the business.dev arrival.
	if !strings.Contains(d.Markdown, `"Rinnovo contratto 2027" — in attesa da 26 ore (oltre le 24h) — business@example.com — #1`) ||
		!strings.Contains(d.Markdown, `"Proposta di collaborazione" — 3 ore fa — business@example.com — #2`) {
		t.Fatalf("digest lines must carry the message id used by `dwt correct` / `dwt mute-thread`:\n%s", d.Markdown)
	}
}

func TestBuild_LateClassifiedMailAppearsInNextDigest(t *testing.T) {
	f := newFx(t)
	// Received Tue 07:59 Rome, classified 08:01 — after Tuesday's 08:00 digest.
	id := f.add(t, "business@example.com", "x@client.example", "Arrivata al limite", "", time.Minute)
	if err := f.s.SaveClassification(context.Background(), store.Classification{MessageID: id, Label: "business.dev",
		Confidence: 0.9, Method: "llm"}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	d, err := Build(context.Background(), f.s, f.cfg, f.tx, at.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Markdown, "Arrivata al limite") {
		t.Fatalf("mail classified after its window closed never reaches a digest:\n%s", d.Markdown)
	}
}

func TestBuild_UsesConfigDigestFlagsNotStaleDB(t *testing.T) {
	f := newFx(t)
	f.add(t, "legal@example.com", "x@client.example", "Riservata legale", "clienti", time.Hour)
	cfg := *f.cfg
	cfg.Mailboxes = append([]config.MailboxConfig(nil), f.cfg.Mailboxes...)
	cfg.Mailboxes[1].Digest = false // owner turned legal@ off; the DB still says digest=1
	d, err := Build(context.Background(), f.s, &cfg, f.tx, at)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Markdown, "Riservata legale") {
		t.Fatal("digest used the stale DB flag instead of the current config")
	}
}

func TestSend_ConcurrentLiveSendsOnce(t *testing.T) {
	f := seed(t)
	var sends atomic.Int32
	_, c := graphtest.New(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // a slow Graph widens the check-then-send window
		sends.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	d, _ := Build(context.Background(), f.s, f.cfg, f.tx, at)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := Send(context.Background(), f.s, c, f.cfg, d, false, at); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := sends.Load(); n != 1 {
		t.Fatalf("concurrent sends delivered %d digests, want 1", n)
	}
}
