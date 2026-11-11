// Package eval replays production classification without saving message,
// classification, thread or queue state. Real LLM usage remains budgeted/stored.
package eval

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/alessiosavi/go-dontwastetime/internal/classify"
	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/pipeline"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

// Gold is one blind-sample reference. Only mailbox and n appear in row outputs.
type Gold struct {
	N                 int      `json:"n"`
	Mailbox           string   `json:"mailbox"`
	InternetMessageID string   `json:"internet_message_id"`
	Accept            []string `json:"accept"`
	NeedsReply        bool     `json:"needs_reply"`
}

var labelKey = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// ReadGold validates the entire input before making any paid calls. Errors
// identify line numbers and fields, never echo input values or email content.
func ReadGold(path string) ([]Gold, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var gold []Gold
	seenN, seenMessage := map[int]bool{}, map[[2]string]bool{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for line := 1; scan.Scan(); line++ {
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			continue
		}
		var wire struct {
			Gold
			Reply *bool `json:"needs_reply"`
		}
		dec := json.NewDecoder(bytes.NewReader(scan.Bytes()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wire); err != nil {
			return nil, fmt.Errorf("eval: invalid gold JSON on line %d", line)
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return nil, fmt.Errorf("eval: extra gold JSON on line %d", line)
		}
		g := wire.Gold
		if g.N <= 0 || !config.ValidAddress(g.Mailbox) || strings.TrimSpace(g.InternetMessageID) == "" || len(g.Accept) == 0 || wire.Reply == nil {
			return nil, fmt.Errorf("eval: missing/invalid required gold field on line %d", line)
		}
		for _, label := range g.Accept {
			if !labelKey.MatchString(label) {
				return nil, fmt.Errorf("eval: invalid accept label on line %d", line)
			}
		}
		key := [2]string{g.Mailbox, g.InternetMessageID}
		if seenN[g.N] || seenMessage[key] {
			return nil, fmt.Errorf("eval: duplicate gold row on line %d", line)
		}
		seenN[g.N], seenMessage[key] = true, true
		g.NeedsReply = *wire.Reply
		gold = append(gold, g)
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("eval: read gold: %w", err)
	}
	if len(gold) == 0 {
		return nil, errors.New("eval: gold file has no rows")
	}
	return gold, nil
}

// Row deliberately excludes IDs, subjects, senders, bodies and rationales.
// Only status=classified rows contribute to metrics; missing/budget rows are
// retained to make the scored denominator auditable.
type Row struct {
	N             int     `json:"n"`
	Mailbox       string  `json:"mailbox"`
	Predicted     string  `json:"predicted"`
	Method        string  `json:"method"`
	Confidence    float64 `json:"confidence"`
	ReplyDecision bool    `json:"reply_decision"`
	Correct       bool    `json:"correct"`
	Status        string  `json:"status"`
}

// Count is an accuracy numerator and denominator (or covered/total).
type Count struct{ Correct, Total int }

// Confusion counts misclassifications using the first acceptable label.
type Confusion struct {
	Expected, Predicted string
	Total               int
}

// Report holds only the metrics and redacted rows, safe for disk/stdout.
type Report struct {
	Rows                          []Row
	Accuracy, Coverage            Count
	ByMailbox, ByMethod, ByBucket map[string]Count
	Confusions                    []Confusion
	ReplyTP, ReplyFP, ReplyFN     int
	Missing, BudgetSkipped        int
	BudgetStopped                 bool
	Usage                         classify.Usage
}

// Run looks up inbound rows without leases, then processes the same batch size
// as the job handler. Its sole database writes are LLM usage in the shared adapter.
func Run(ctx context.Context, s *store.Store, c *graph.Client, l *classify.LLM, t *classify.Taxonomy, cfg *config.Config, gold []Gold) (Report, error) {
	r := Report{Rows: make([]Row, len(gold))}
	var messages []store.MessageRow
	byID := map[int64]int{}
	for i, g := range gold {
		r.Rows[i] = Row{N: g.N, Mailbox: g.Mailbox, Status: "pending"}
		m, err := s.InboundMessage(ctx, g.Mailbox, g.InternetMessageID)
		if errors.Is(err, sql.ErrNoRows) {
			r.Rows[i].Status = "missing"
			r.Missing++
			continue
		}
		if err != nil {
			return r, fmt.Errorf("eval: lookup row %d: %w", g.N, err)
		}
		byID[m.ID] = i
		messages = append(messages, m)
	}
	// Match ClaimUnclassified's ordering so gold-file order does not change
	// which messages share an LLM request. Output stays in gold-file order.
	slices.SortFunc(messages, func(a, b store.MessageRow) int {
		if n := a.ReceivedAt.Compare(b.ReceivedAt); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	for start := 0; start < len(messages); start += pipeline.ClassifyBatch {
		end := min(start+pipeline.ClassifyBatch, len(messages))
		u, err := pipeline.ClassifyMessages(ctx, s, c, l, t, cfg, messages[start:end], false, func(batch []pipeline.ClassifiedMessage) error {
			for _, item := range batch {
				cl, m := item.Classification, item.Message
				i := byID[m.ID]
				row := &r.Rows[i]
				row.Predicted, row.Method, row.Confidence, row.Status = cl.Label, cl.Method, cl.Confidence, "classified"
				row.Correct = slices.Contains(gold[i].Accept, cl.Label)
				row.ReplyDecision = threads.ReplyDecision(cl.Label, &store.ThreadMessage{
					From: m.From, Subject: m.Subject, Preview: m.BodyPreview, IsAuto: m.IsAuto, IsBulk: m.IsBulk, IsList: m.IsList,
					Urgent: cl.Urgent, Negative: cl.Negative, ReplyExpected: cl.ReplyExpected,
				})
			}
			return nil
		})
		r.Usage.In, r.Usage.Out = r.Usage.In+u.In, r.Usage.Out+u.Out
		if errors.Is(err, store.ErrBudgetReached) {
			r.BudgetStopped = true
			break
		}
		if err != nil {
			return r, err
		}
	}
	for i := range r.Rows {
		if r.Rows[i].Status == "pending" {
			r.Rows[i].Status = "budget_skipped"
			r.BudgetSkipped++
		}
	}
	r.score(gold)
	return r, nil
}

func (r *Report) score(gold []Gold) {
	r.ByMailbox, r.ByMethod, r.ByBucket = map[string]Count{}, map[string]Count{}, map[string]Count{}
	confusions := map[[2]string]int{}
	for i, row := range r.Rows {
		if row.Status != "classified" {
			continue
		}
		g := gold[i]
		addCount(&r.Accuracy, row.Correct)
		addCount(&r.Coverage, row.Predicted != classify.Unsure)
		for _, group := range []struct {
			m   map[string]Count
			key string
		}{{r.ByMailbox, row.Mailbox}, {r.ByMethod, row.Method}, {r.ByBucket, g.Accept[0]}} {
			v := group.m[group.key]
			addCount(&v, row.Correct)
			group.m[group.key] = v
		}
		if !row.Correct {
			confusions[[2]string{g.Accept[0], row.Predicted}]++
		}
		switch {
		case row.ReplyDecision && g.NeedsReply:
			r.ReplyTP++
		case row.ReplyDecision:
			r.ReplyFP++
		case g.NeedsReply:
			r.ReplyFN++
		}
	}
	for pair, n := range confusions {
		r.Confusions = append(r.Confusions, Confusion{pair[0], pair[1], n})
	}
	slices.SortFunc(r.Confusions, func(a, b Confusion) int {
		if a.Total != b.Total {
			return b.Total - a.Total
		}
		if n := strings.Compare(a.Expected, b.Expected); n != 0 {
			return n
		}
		return strings.Compare(a.Predicted, b.Predicted)
	})
	if len(r.Confusions) > 10 {
		r.Confusions = r.Confusions[:10]
	}
}

func addCount(c *Count, correct bool) {
	c.Total++
	if correct {
		c.Correct++
	}
}
