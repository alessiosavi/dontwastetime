// Package llmtest is a fake OpenAI-compatible /chat/completions endpoint for
// tests. Import it from _test.go files only.
package llmtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Item is one message the classifier sent (mirrors classify's prompt shape).
type Item struct {
	ID      int64  `json:"id"`
	Mailbox string `json:"mailbox"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Preview string `json:"preview"`
	Bulk    bool   `json:"bulk"`
}

// Request is what the fake saw.
type Request struct {
	JSONMode bool
	Items    []Item // parsed from the last user message when it is a classify call
	Raw      map[string]any
}

// Reply is what the fake answers: a status (0 = 200) and the assistant
// message content, plus token usage.
type Reply struct {
	Status  int
	Content string
	In, Out int
}

// Server is the fake endpoint. Respond decides each answer.
type Server struct {
	URL     string
	mu      sync.Mutex
	Calls   []Request
	Respond func(Request) Reply
}

// New starts the fake.
func New(t testing.TB, respond func(Request) Reply) *Server {
	t.Helper()
	f := &Server{Respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		req := Request{Raw: body}
		_, req.JSONMode = body["response_format"]
		if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
			last, _ := msgs[len(msgs)-1].(map[string]any)
			content, _ := last["content"].(string)
			var payload struct {
				Messages []Item `json:"messages"`
			}
			if json.Unmarshal([]byte(content), &payload) == nil {
				req.Items = payload.Messages
			}
		}
		f.mu.Lock()
		f.Calls = append(f.Calls, req)
		respond := f.Respond
		f.mu.Unlock()
		rep := respond(req)
		if rep.Status != 0 && rep.Status != http.StatusOK {
			w.WriteHeader(rep.Status)
			_, _ = w.Write([]byte(`{"error":{"message":"fake error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": rep.Content}}},
			"usage":   map[string]int{"prompt_tokens": rep.In, "completion_tokens": rep.Out},
		})
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// CallCount returns the number of requests served.
func (f *Server) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// Answer is one result in the fake's JSON reply.
type Answer struct {
	ID            int64   `json:"id"`
	Label         string  `json:"label"`
	Confidence    float64 `json:"confidence"`
	Entity        string  `json:"entity,omitempty"`
	Urgent        bool    `json:"urgent,omitempty"`
	Negative      bool    `json:"negative,omitempty"`
	Reason        string  `json:"reason"`
	ReplyExpected *bool   `json:"reply_expected,omitempty"`
}

// Results renders answers as the {"results":[...]} object classify expects.
func Results(answers ...Answer) string {
	b, _ := json.Marshal(map[string]any{"results": answers})
	return string(b)
}

// Label answers every item with label at the given confidence.
func Label(label string, confidence float64) func(Request) Reply {
	return func(r Request) Reply {
		var as []Answer
		for _, it := range r.Items {
			as = append(as, Answer{ID: it.ID, Label: label, Confidence: confidence, Reason: "fake"})
		}
		return Reply{Content: Results(as...), In: 100, Out: 20}
	}
}
