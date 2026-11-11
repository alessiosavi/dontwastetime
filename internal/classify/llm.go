package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/store"
)

// maxExamples bounds the few-shot corrections in the prompt.
const maxExamples = 20

// Item is one message sent to the classifier. Preview is a bounded body
// excerpt (bodies leave the machine only through this call).
type Item struct {
	ID      int64  `json:"id"`
	Mailbox string `json:"mailbox"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Preview string `json:"preview"`
	Bulk    bool   `json:"bulk"`
}

// Usage is token usage of one or more calls.
type Usage struct{ In, Out int }

// LLM is an OpenAI-compatible chat-completions client with global daily token
// and monthly cost budgets shared by every LLM stage.
type LLM struct {
	cfg      config.LLMConfig
	s        *store.Store
	now      func() time.Time
	http     *http.Client
	noJSON   atomic.Bool // endpoint rejected response_format once
	entities map[string]bool
}

// NewLLM builds the adapter. Usage is read from and recorded in s.
func NewLLM(cfg config.LLMConfig, s *store.Store, now func() time.Time) *LLM {
	return &LLM{cfg: cfg, s: s, now: now, http: &http.Client{Timeout: 120 * time.Second},
		entities: map[string]bool{"SRL": true, "LTD": true, "Branch": true}}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// errBadRequest marks a 400 so JSON mode can be dropped once.
var errBadRequest = errors.New("llm: 400 bad request")

// checkBudget refuses before any call once the daily token or monthly cost
// threshold is reached. In-flight calls can still settle above either cap.
func (l *LLM) checkBudget(ctx context.Context) error {
	now := l.now()
	in, out, err := l.s.UsageToday(ctx, config.Day(now))
	if err != nil {
		return err
	}
	if in+out >= l.cfg.DailyMaxTokens {
		return store.ErrBudgetReached
	}
	if l.cfg.MonthlyMaxCost > 0 {
		in, out, err := l.s.UsageMonth(ctx, config.Month(now))
		if err != nil {
			return err
		}
		if l.cfg.Cost(in, out) >= l.cfg.MonthlyMaxCost {
			return store.ErrBudgetReached
		}
	}
	return nil
}

// chat reserves estimated input before calling, then settles on actual usage.
// Failed calls retain their reservation, including after caller cancellation.
func (l *LLM) chat(ctx context.Context, msgs []chatMessage, maxTokens int, jsonMode bool) (string, Usage, error) {
	if err := l.checkBudget(ctx); err != nil {
		return "", Usage{}, err
	}
	body := map[string]any{"model": l.cfg.Model, "messages": msgs, "temperature": 0}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	if jsonMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.cfg.URL, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.cfg.APIKey)
	req.Header.Set("api-key", l.cfg.APIKey) // Azure AI Foundry key header
	day := config.Day(l.now())
	reserved := Usage{In: len(raw) / 4}
	if err := l.s.UsageAdd(ctx, day, reserved.In, 0); err != nil {
		return "", Usage{}, err
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return "", reserved, fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", reserved, fmt.Errorf("llm: read response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return "", reserved, errBadRequest
	case resp.StatusCode != http.StatusOK:
		return "", reserved, fmt.Errorf("llm: HTTP %d", resp.StatusCode)
	}
	var cr chatResponse
	if err := json.Unmarshal(payload, &cr); err != nil || len(cr.Choices) == 0 {
		return "", reserved, fmt.Errorf("llm: malformed response envelope")
	}
	u := Usage{In: cr.Usage.PromptTokens, Out: cr.Usage.CompletionTokens}
	if err := l.s.UsageAdjust(ctx, day, u.In-reserved.In, u.Out); err != nil {
		return "", u, err
	}
	return cr.Choices[0].Message.Content, u, nil
}

// chatJSON uses JSON mode when the endpoint supports it; a 400 drops it for
// this and every later call.
func (l *LLM) chatJSON(ctx context.Context, msgs []chatMessage) (string, Usage, error) {
	var reserved Usage
	if !l.noJSON.Load() {
		out, u, err := l.chat(ctx, msgs, 0, true)
		if !errors.Is(err, errBadRequest) {
			return out, u, err
		}
		l.noJSON.Store(true)
		reserved = u
	}
	out, u, err := l.chat(ctx, msgs, 0, false)
	u.In += reserved.In
	u.Out += reserved.Out
	return out, u, err
}

// Complete runs a free-form prompt (explore mode). Budget-checked.
func (l *LLM) Complete(ctx context.Context, prompt string, maxTokens int) (string, Usage, error) {
	return l.chat(ctx, []chatMessage{{Role: "user", Content: prompt}}, maxTokens, false)
}

type answer struct {
	ID            int64           `json:"id"`
	Label         string          `json:"label"`
	Confidence    float64         `json:"confidence"`
	Entity        string          `json:"entity"`
	Urgent        bool            `json:"urgent"`
	Negative      bool            `json:"negative"`
	Reason        string          `json:"reason"`
	ReplyExpected json.RawMessage `json:"reply_expected"`
}

// Classify labels a batch (≤10 recommended) in one call; a malformed answer
// is retried once. Every input item gets a Result: unknown labels, low
// confidence and missing answers become `unsure`.
func (l *LLM) Classify(ctx context.Context, batch []Item, t *Taxonomy, examples []store.Correction) ([]Result, Usage, error) {
	user, err := json.Marshal(map[string]any{"messages": batch})
	if err != nil {
		return nil, Usage{}, err
	}
	msgs := []chatMessage{{Role: "system", Content: systemPrompt(t, examples)}, {Role: "user", Content: string(user)}}
	var total Usage
	var answers []answer
	for attempt := 0; ; attempt++ {
		out, u, err := l.chatJSON(ctx, msgs)
		total.In, total.Out = total.In+u.In, total.Out+u.Out
		if err != nil {
			return nil, total, err
		}
		if answers, err = parseAnswers(out); err == nil {
			break
		}
		if attempt == 1 {
			return nil, total, fmt.Errorf("llm: malformed answer twice: %w", err)
		}
	}
	byID := make(map[int64]answer, len(answers))
	for _, a := range answers {
		byID[a.ID] = a
	}
	res := make([]Result, len(batch))
	for i, it := range batch {
		a, ok := byID[it.ID]
		r := Result{ID: it.ID, Label: a.Label, Confidence: a.Confidence, Urgent: a.Urgent, Negative: a.Negative,
			Method: MethodLLM, Rationale: a.Reason}
		// A missing, null or wrongly typed field preserves legacy label fallback.
		var reply *bool
		if json.Unmarshal(a.ReplyExpected, &reply) == nil {
			r.ReplyExpected = reply
		}
		if l.entities[a.Entity] {
			r.Entity = a.Entity
		}
		switch {
		case !ok:
			r.Label, r.Rationale = Unsure, "nessuna risposta dal classificatore"
		case !t.Has(a.Label):
			r.Label, r.Rationale = Unsure, withReason(fmt.Sprintf("etichetta sconosciuta %q", a.Label), a.Reason)
		case a.Confidence < l.cfg.ConfidenceThreshold:
			r.Label, r.Rationale = Unsure, withReason(fmt.Sprintf("confidenza bassa (%.2f) per %s", a.Confidence, a.Label), a.Reason)
		}
		res[i] = r
	}
	return res, total, nil
}

func withReason(head, reason string) string {
	if reason = strings.TrimSpace(reason); reason != "" {
		return head + ": " + reason
	}
	return head
}

// parseAnswers accepts {"results":[...]}, a bare array, or either wrapped in
// prose / code fences.
func parseAnswers(s string) ([]answer, error) {
	s = strings.TrimSpace(s)
	try := func(js string) ([]answer, bool) {
		var obj struct {
			Results []answer `json:"results"`
		}
		if json.Unmarshal([]byte(js), &obj) == nil && obj.Results != nil {
			return obj.Results, true
		}
		var arr []answer
		if json.Unmarshal([]byte(js), &arr) == nil && arr != nil {
			return arr, true
		}
		return nil, false
	}
	if a, ok := try(s); ok {
		return a, nil
	}
	for _, pair := range [][2]string{{"{", "}"}, {"[", "]"}} {
		i, j := strings.Index(s, pair[0]), strings.LastIndex(s, pair[1])
		if i >= 0 && j > i {
			if a, ok := try(s[i : j+1]); ok {
				return a, nil
			}
		}
	}
	return nil, errors.New("no JSON results in answer")
}

func systemPrompt(t *Taxonomy, examples []store.Correction) string {
	var b strings.Builder
	b.WriteString("You classify business emails of an Italian company (Example) into exactly one label (\"cassetto\") each.\n")
	b.WriteString("bulk: true means the mail was sent in bulk (unsubscribe header). Newsletters and promotions are marketing.news, but receipts, payment notices, product or system reports and alerts sent in bulk keep their own label (admin.invoices, notifications, tech.alerts).\n")
	b.WriteString("reply_expected: true only if someone at the company is expected to answer this message by email (a real person asking us something or waiting for a response or action); false for automated mail, newsletters, receipts, FYI or cc-only mail, confirmations, and messages that close a conversation.\n")
	b.WriteString("Labels:\n")
	for _, k := range t.Order {
		l := t.Labels[k]
		fmt.Fprintf(&b, "- %s: %s", k, l.Description)
		if len(l.Signals) > 0 {
			fmt.Fprintf(&b, " Signals: %s.", strings.Join(l.Signals, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString(`Rules: collaborator invoices go to personale.collaborators and event-venue invoices to eventi.post, not admin.invoices. ` +
		`If you are not confident, use "unsure" or a low confidence. ` +
		`Set "urgent" true only for explicit urgency or deadlines (urgente, entro, ASAP, scadenza, bloccante); ` +
		`"negative" true for complaints or escalations; "entity" only SRL, LTD or Branch when the mail is addressed to that company.` + "\n")
	if n := min(len(examples), maxExamples); n > 0 {
		b.WriteString("Human corrections (follow these):\n")
		for _, e := range examples[:n] {
			fmt.Fprintf(&b, "- from %s, subject %q → %s\n", e.From, e.Subject, e.NewLabel)
		}
	}
	b.WriteString(`Answer with JSON only: {"results":[{"id":<id>,"label":"<label>","confidence":<0..1>,"entity":"","urgent":false,"negative":false,"reply_expected":<true|false>,"reason":"<short, in Italian>"}]} with one result per input message.`)
	return b.String()
}
