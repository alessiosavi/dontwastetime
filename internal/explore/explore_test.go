package explore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/classify/llmtest"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/graph/graphtest"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

var now0 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

const mb = "marketing@example.com"

type env struct {
	s     *store.Store
	mail  *graphtest.MailFake
	c     *graph.Client
	llm   *llmtest.Server
	l     *classify.LLM
	tx    *classify.Taxonomy
	out   string
	seen  [][]sentGroup // groups sent per LLM call
	reply func(gs []sentGroup) string
}

// sentGroup mirrors the JSON lines explore sends after "GROUPS:".
type sentGroup struct {
	ID      int    `json:"id"`
	N       int    `json:"n"`
	Subject string `json:"subject"`
}

func newEnv(t *testing.T) *env {
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
	e := &env{s: s, tx: tx, out: t.TempDir()}
	e.mail, _, e.c = graphtest.NewMail(t)
	// Default answer: every sentGroup in one "newsletter" cluster, except groups
	// whose subject contains "?" which go to "doesn't fit".
	e.reply = func(gs []sentGroup) string {
		var fit, nofit []int
		for _, g := range gs {
			if strings.Contains(g.Subject, "?") {
				nofit = append(nofit, g.ID)
			} else {
				fit = append(fit, g.ID)
			}
		}
		b, _ := json.Marshal(map[string]any{"clusters": []map[string]any{
			{"name": "Newsletter", "label": "marketing.news", "description": "promo", "groups": fit},
			{"name": "Varie", "label": "doesn't fit", "description": "altro", "groups": nofit},
		}})
		return string(b)
	}
	e.llm = llmtest.New(t, func(r llmtest.Request) llmtest.Reply {
		msgs := r.Raw["messages"].([]any)
		prompt := msgs[len(msgs)-1].(map[string]any)["content"].(string)
		_, payload, _ := strings.Cut(prompt, "GROUPS:\n")
		var gs []sentGroup
		if err := json.Unmarshal([]byte(payload), &gs); err != nil {
			t.Errorf("explore prompt payload: %v", err)
		}
		e.seen = append(e.seen, gs)
		return llmtest.Reply{Content: e.reply(gs), In: 100, Out: 50}
	})
	e.l = classify.NewLLM(config.LLMConfig{URL: e.llm.URL, Model: "m", APIKey: "k", DailyMaxTokens: 100_000, ConfidenceThreshold: 0.7},
		s, func() time.Time { return now0 })
	return e
}

func (e *env) add(subject, from, body string, ageHours int) {
	n := len(e.mail.Boxes[mb]["inbox"]) + 1
	e.mail.Add(graph.Message{ID: fmt.Sprintf("g%d", n), InternetMessageID: fmt.Sprintf("<%d@t>", n), Mailbox: mb, Folder: "inbox",
		Subject: subject, From: from, BodyText: body, ReceivedAt: now0.Add(-time.Duration(ageHours) * time.Hour)})
}

func (e *env) run(t *testing.T, o Options) ([]string, error) {
	t.Helper()
	if o.Mailboxes == nil {
		o.Mailboxes = []string{mb}
	}
	return Run(context.Background(), e.c, e.s, e.l, e.tx, o, e.out, now0)
}

func TestExplore_DedupBeforeLLM(t *testing.T) {
	e := newEnv(t)
	e.add("Le novità di novembre", "news@brand.example", "Scopri le offerte della settimana nel nostro negozio online", 5)
	e.add("Le novità di novembre", "news@brand.example", "Scopri le offerte della settimana nel nostro negozio online", 3)
	e.add("Possiamo sentirci?", "mario@client.example", "Volevo chiederti una cosa sul contratto", 2)
	if _, err := e.run(t, Options{Days: 90, MaxMessages: 5000}); err != nil {
		t.Fatal(err)
	}
	if len(e.seen) != 1 || len(e.seen[0]) != 2 {
		t.Fatalf("LLM saw %v, want one call with 2 groups", e.seen)
	}
	var dup sentGroup
	for _, g := range e.seen[0] {
		if g.Subject == "Le novità di novembre" {
			dup = g
		}
	}
	if dup.N != 2 {
		t.Fatalf("identical newsletters not collapsed: %+v", e.seen[0])
	}
}

func TestExplore_MaxMessagesStops(t *testing.T) {
	e := newEnv(t)
	for i := range 10 {
		e.add(fmt.Sprintf("Oggetto diverso numero %d", i), fmt.Sprintf("x%d@d%d.example", i, i), fmt.Sprintf("corpo unico %d alfa beta gamma %d", i, i*7), i+1)
	}
	paths, err := e.run(t, Options{Days: 90, MaxMessages: 4})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, gs := range e.seen {
		for _, g := range gs {
			total += g.N
		}
	}
	if total != 4 {
		t.Fatalf("messages analysed = %d, want the cap 4", total)
	}
	report := read(t, paths[0])
	if !strings.Contains(report, "Messaggi letti: 4") || !strings.Contains(report, "limite") {
		t.Fatalf("report does not state the cap:\n%s", report)
	}
}

func TestExplore_ReportContainsCoverageAndUnsure(t *testing.T) {
	e := newEnv(t)
	e.add("Newsletter A", "a@brand.example", "promo uno due tre quattro", 5)
	e.add("Newsletter B", "b@brand.example", "sconti cinque sei sette otto", 4)
	e.add("Newsletter C", "c@brand.example", "eventi nove dieci undici dodici", 3)
	e.add("Una domanda?", "q@client.example", "come funziona il servizio", 2)
	paths, err := e.run(t, Options{Days: 90, MaxMessages: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("want a per-mailbox report and a summary, got %v", paths)
	}
	report := read(t, paths[0])
	for _, want := range []string{"Newsletter", "marketing.news", "Copertura tassonomia: 75%", "Non rientra", "Una domanda?"} {
		if !strings.Contains(report, want) {
			t.Errorf("mailbox report lacks %q:\n%s", want, report)
		}
	}
	if summary := read(t, paths[1]); !strings.Contains(summary, "marketing.news") || !strings.Contains(summary, "75%") {
		t.Errorf("summary:\n%s", summary)
	}
	if !strings.HasPrefix(filepath.Base(paths[0]), "explore-marketing@example.com-2026-11-10") {
		t.Errorf("report name %s", paths[0])
	}
}

func TestExplore_BudgetReachedStopsCleanly(t *testing.T) {
	e := newEnv(t)
	e.add("Newsletter A", "a@brand.example", "promo", 5)
	_ = e.s.UsageAdd(context.Background(), config.Day(now0), 100_000, 0)
	paths, err := e.run(t, Options{Days: 90, MaxMessages: 5000})
	if !errors.Is(err, store.ErrBudgetReached) {
		t.Fatalf("want ErrBudgetReached, got %v", err)
	}
	if len(paths) == 0 || !strings.Contains(read(t, paths[0]), "budget") {
		t.Fatalf("partial report missing or silent about the budget stop: %v", paths)
	}
	if report := read(t, paths[0]); !strings.Contains(report, "token giornalieri o costo mensile") {
		t.Fatalf("budget stop must name both caps:\n%s", report)
	}
	if e.llm.CallCount() != 0 {
		t.Fatal("budget-stopped explore called the LLM")
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
