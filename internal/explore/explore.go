// Package explore is phase 0: read a window of real mail, collapse
// near-duplicates, let the LLM propose clusters, and write Markdown reports
// mapping them onto the taxonomy for Operator + founder sign-off. Read-only:
// nothing is stored except the reports, LLM usage and one action-log row.
package explore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alessiosavi/GoGPUtils/minhash"
	"github.com/alessiosavi/GoGPUtils/sliceutil"
	"github.com/alessiosavi/GoGPUtils/stringutil"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/pipeline"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

const (
	defaultDays        = 90
	defaultMaxMessages = 5000
	groupsPerCall      = 60
	previewLen         = 200
	numPerm            = 128
	nearDupThreshold   = 0.8
	noFit              = "doesn't fit"
	maxAnswerTokens    = 4000
)

// Options bound one explore run.
type Options struct {
	Days, MaxMessages int
	Mailboxes         []string
}

type group struct {
	ID         int    `json:"id"`
	N          int    `json:"n"`
	Subject    string `json:"subject"`
	FromDomain string `json:"from_domain"`
	Preview    string `json:"preview"`
	domains    map[string]int
	sig        minhash.Signature
}

type cluster struct {
	Name, Label, Description string
	Count                    int
	groups                   []*group
}

type report struct {
	Mailbox  string
	Fetched  int
	Capped   bool
	Groups   int
	Calls    int
	Tokens   int
	Clusters []*cluster
	Stopped  string // why the run stopped early
	Err      string
}

func (r *report) covered() int {
	n := 0
	for _, c := range r.Clusters {
		if c.Label != noFit && c.Label != classify.Unsure {
			n += c.Count
		}
	}
	return n
}

// Run explores each mailbox's inbox over the last o.Days days and returns
// the written report paths (one per mailbox, then the combined summary).
// It stops at o.MaxMessages (total) and on the daily token or monthly cost cap;
// a budget stop still writes partial reports and returns an error wrapping
// store.ErrBudgetReached.
func Run(ctx context.Context, c *graph.Client, s *store.Store, l *classify.LLM, t *classify.Taxonomy, o Options, outDir string, now time.Time) ([]string, error) {
	if o.Days <= 0 {
		o.Days = defaultDays
	}
	if o.MaxMessages <= 0 {
		o.MaxMessages = defaultMaxMessages
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, fmt.Errorf("explore: %w", err)
	}
	day := config.Day(now)
	remaining := o.MaxMessages
	var reports []*report
	var paths []string
	var stopErr error
	for _, mb := range o.Mailboxes {
		r := &report{Mailbox: mb}
		reports = append(reports, r)
		if remaining <= 0 {
			r.Stopped = "limite --max-messages raggiunto prima di questa casella"
		} else if err := exploreMailbox(ctx, c, l, t, o, r, &remaining, now); err != nil {
			if errors.Is(err, store.ErrBudgetReached) {
				r.Stopped = "interrotto: budget LLM raggiunto (token giornalieri o costo mensile; risultati parziali)"
				stopErr = fmt.Errorf("explore %s: %w", mb, err)
			} else {
				r.Err = err.Error()
			}
		}
		p := filepath.Join(outDir, fmt.Sprintf("explore-%s-%s.md", mb, day))
		if err := os.WriteFile(p, []byte(renderMailbox(r, o, now)), 0o600); err != nil {
			return paths, fmt.Errorf("explore: %w", err)
		}
		paths = append(paths, p)
		if stopErr != nil {
			break
		}
	}
	p := filepath.Join(outDir, "explore-summary-"+day+".md")
	if err := os.WriteFile(p, []byte(renderSummary(reports, day)), 0o600); err != nil {
		return paths, fmt.Errorf("explore: %w", err)
	}
	paths = append(paths, p)
	_ = s.LogAction(ctx, "explore", "explore_run", strings.Join(o.Mailboxes, ","),
		map[string]any{"days": o.Days, "max_messages": o.MaxMessages, "reports": len(paths), "stopped": stopErr != nil}, now)
	return paths, stopErr
}

func exploreMailbox(ctx context.Context, c *graph.Client, l *classify.LLM, t *classify.Taxonomy, o Options, r *report, remaining *int, now time.Time) error {
	msgs, err := c.ListMessages(ctx, r.Mailbox, store.FolderInbox, now.AddDate(0, 0, -o.Days), graph.ListOptions{
		Top: 100, MaxPages: (*remaining + 99) / 100, Truncated: func(int) { r.Capped = true },
	})
	if err != nil {
		return err
	}
	if len(msgs) > *remaining {
		msgs, r.Capped = msgs[:*remaining], true
	}
	*remaining -= len(msgs)
	r.Fetched = len(msgs)
	groups, err := dedup(msgs)
	if err != nil {
		return err
	}
	r.Groups = len(groups)
	byID := map[int]*group{}
	for _, g := range groups {
		byID[g.ID] = g
	}
	merged := map[string]*cluster{}
	for _, chunk := range sliceutil.Chunk(groups, groupsPerCall) {
		out, u, err := l.Complete(ctx, prompt(r.Mailbox, t, chunk), maxAnswerTokens)
		if err != nil {
			r.Clusters = sorted(merged)
			return err
		}
		r.Calls++
		r.Tokens += u.In + u.Out
		mergeAnswer(merged, out, chunk, byID, t)
	}
	r.Clusters = sorted(merged)
	return nil
}

// dedup collapses near-identical messages (MinHash over subject+preview
// word bigrams, LSH candidates, similarity ≥ 0.8) before any LLM call.
func dedup(msgs []graph.Message) ([]*group, error) {
	h, err := minhash.New(numPerm, 1)
	if err != nil {
		return nil, err
	}
	bands, rows, err := minhash.Bands(numPerm, nearDupThreshold)
	if err != nil {
		return nil, err
	}
	idx, err := minhash.NewIndex(bands, rows)
	if err != nil {
		return nil, err
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, m := range msgs {
		preview := stringutil.Truncate(stringutil.NormalizeWhitespace(m.BodyText), previewLen, "…")
		text := pipeline.NormalizeSubject(m.Subject + " " + preview)
		dom := domainOf(m.From)
		var g *group
		if sig, ok := signature(h, text); ok {
			cands, _ := idx.Candidates(sig)
			for _, id := range cands {
				if cand := byKey[id]; cand != nil {
					if sim, _ := minhash.Similarity(sig, cand.sig); sim >= nearDupThreshold {
						g = cand
						break
					}
				}
			}
			if g == nil {
				g = &group{ID: len(groups) + 1, Subject: m.Subject, FromDomain: dom, Preview: preview, domains: map[string]int{}, sig: sig}
				groups = append(groups, g)
				key := fmt.Sprint(g.ID)
				byKey[key] = g
				_ = idx.Add(key, sig)
			}
		} else {
			g = &group{ID: len(groups) + 1, Subject: m.Subject, FromDomain: dom, Preview: preview, domains: map[string]int{}}
			groups = append(groups, g)
		}
		g.N++
		g.domains[dom]++
	}
	return groups, nil
}

func signature(h *minhash.Hasher, text string) (minhash.Signature, bool) {
	tokens := strings.Fields(text)
	k := 2
	if len(tokens) < 2 {
		k = 1
	}
	if len(tokens) == 0 {
		return nil, false
	}
	sh, err := minhash.Shingles(tokens, k)
	if err != nil {
		return nil, false
	}
	sig, err := h.Sign(sh)
	return sig, err == nil
}

func prompt(mailbox string, t *classify.Taxonomy, groups []*group) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sei un analista di posta aziendale. Questi sono gruppi di email (già deduplicati) della casella %s.\n", mailbox)
	b.WriteString("Raggruppali in 8–15 cluster tematici (meno se i gruppi sono pochi). Per ogni cluster: un nome breve in italiano, ")
	b.WriteString("il cassetto della tassonomia che corrisponde (uno di: ")
	b.WriteString(strings.Join(t.Order, ", "))
	fmt.Fprintf(&b, ") oppure %q se nessuno corrisponde, una descrizione breve, e gli id dei gruppi. Ogni gruppo in un solo cluster.\n", noFit)
	b.WriteString(`Rispondi solo con JSON: {"clusters":[{"name":"...","label":"...","description":"...","groups":[1,2]}]}` + "\n")
	payload, _ := json.Marshal(groups)
	b.WriteString("GROUPS:\n")
	b.Write(payload)
	return b.String()
}

func mergeAnswer(merged map[string]*cluster, out string, chunk []*group, byID map[int]*group, t *classify.Taxonomy) {
	var ans struct {
		Clusters []struct {
			Name, Label, Description string
			Groups                   []int
		} `json:"clusters"`
	}
	if i, j := strings.Index(out, "{"), strings.LastIndex(out, "}"); i >= 0 && j > i {
		_ = json.Unmarshal([]byte(out[i:j+1]), &ans)
	}
	assigned := map[int]bool{}
	add := func(name, label, desc string, g *group) {
		if !t.Has(label) {
			label = noFit
		}
		key := strings.ToLower(strings.TrimSpace(name)) + "|" + label
		cl := merged[key]
		if cl == nil {
			cl = &cluster{Name: name, Label: label, Description: desc}
			merged[key] = cl
		}
		cl.groups = append(cl.groups, g)
		cl.Count += g.N
		assigned[g.ID] = true
	}
	for _, c := range ans.Clusters {
		for _, id := range c.Groups {
			if g := byID[id]; g != nil && !assigned[id] {
				add(c.Name, c.Label, c.Description, g)
			}
		}
	}
	for _, g := range chunk {
		if !assigned[g.ID] {
			add("Non assegnati dal modello", noFit, "", g)
		}
	}
}

func sorted(m map[string]*cluster) []*cluster {
	return slices.SortedFunc(maps.Values(m), func(a, b *cluster) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Name, b.Name))
	})
}

func pct(n, total int) int {
	if total == 0 {
		return 0
	}
	return n * 100 / total
}

func labelText(l string) string {
	if l == noFit {
		return "Non rientra"
	}
	return l
}

func renderMailbox(r *report, o Options, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Esplorazione casella %s — %s\n\n", r.Mailbox, config.Day(now))
	fmt.Fprintf(&b, "Finestra: ultimi %d giorni (dal %s). Messaggi letti: %d", o.Days, config.Day(now.AddDate(0, 0, -o.Days)), r.Fetched)
	if r.Capped {
		fmt.Fprintf(&b, " (limite --max-messages %d raggiunto)", o.MaxMessages)
	}
	fmt.Fprintf(&b, ". Gruppi dopo deduplica: %d. Chiamate LLM: %d (%d token).\n\n", r.Groups, r.Calls, r.Tokens)
	if r.Stopped != "" {
		fmt.Fprintf(&b, "**%s**\n\n", r.Stopped)
	}
	if r.Err != "" {
		fmt.Fprintf(&b, "**Errore:** %s\n\n", r.Err)
	}
	fmt.Fprintf(&b, "Copertura tassonomia: %d%% · Non rientra: %d%%\n\n", pct(r.covered(), r.Fetched), pct(r.Fetched-r.covered(), r.Fetched))
	b.WriteString("| Cluster | Cassetto proposto | Messaggi | % | Oggetti rappresentativi | Mittenti | Esito (approva / rinomina / rifiuta) |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, c := range r.Clusters {
		fmt.Fprintf(&b, "| %s | %s | %d | %d%% | %s | %s | |\n", cell(c.Name), labelText(c.Label), c.Count, pct(c.Count, r.Fetched),
			cell(strings.Join(topSubjects(c), "; ")), cell(strings.Join(topDomains(c), ", ")))
	}
	b.WriteString("\nFirma per il via libera al classificatore: Operator ______  Fondatore ______\n")
	return b.String()
}

func renderSummary(reports []*report, day string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Esplorazione — riepilogo %s\n\n| Casella | Messaggi | Copertura | Note |\n|---|---|---|---|\n", day)
	byLabel := map[string]int{}
	total, covered := 0, 0
	for _, r := range reports {
		note := r.Stopped
		if r.Err != "" {
			note = "errore: " + r.Err
		}
		fmt.Fprintf(&b, "| %s | %d | %d%% | %s |\n", r.Mailbox, r.Fetched, pct(r.covered(), r.Fetched), cell(note))
		total, covered = total+r.Fetched, covered+r.covered()
		for _, c := range r.Clusters {
			byLabel[c.Label] += c.Count
		}
	}
	b.WriteString("\n## Per cassetto proposto\n\n| Cassetto | Messaggi | % |\n|---|---|---|\n")
	for _, l := range slices.Sorted(maps.Keys(byLabel)) {
		fmt.Fprintf(&b, "| %s | %d | %d%% |\n", labelText(l), byLabel[l], pct(byLabel[l], total))
	}
	fmt.Fprintf(&b, "\nCopertura complessiva: %d%%\n\n", pct(covered, total))
	b.WriteString("Regola: ripetere l'esplorazione ogni mese per 3 mesi, o quando \"Da classificare\" supera il 15%.\n")
	return b.String()
}

func topSubjects(c *cluster) []string {
	gs := slices.SortedFunc(slices.Values(c.groups), func(a, b *group) int { return cmp.Compare(b.N, a.N) })
	var out []string
	for _, g := range sliceutil.Take(gs, 3) {
		out = append(out, stringutil.Truncate(g.Subject, 60, "…"))
	}
	return out
}

func topDomains(c *cluster) []string {
	counts := map[string]int{}
	for _, g := range c.groups {
		for d, n := range g.domains {
			counts[d] += n
		}
	}
	ds := slices.SortedFunc(maps.Keys(counts), func(a, b string) int { return cmp.Or(cmp.Compare(counts[b], counts[a]), cmp.Compare(a, b)) })
	return sliceutil.Take(ds, 3)
}

func cell(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "|", "/"), "\n", " ") }

func domainOf(addr string) string {
	_, d, _ := strings.Cut(strings.ToLower(addr), "@")
	return d
}
