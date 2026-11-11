package graphtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/graph"
)

// SentMail is one captured sendMail call.
type SentMail struct {
	Sender  string
	Payload map[string]any
}

// MailFake is a stateful fake of the mail endpoints dwt uses. Configure the
// exported maps before (or between) calls; it is safe for concurrent use.
type MailFake struct {
	mu sync.Mutex
	// Boxes: mailbox → folder → messages (ReceivedAt drives $filter/order).
	Boxes map[string]map[string][]graph.Message
	// Forbidden mailboxes answer 403 on every call.
	Forbidden map[string]bool
	// Throttle: mailbox → page index (0-based) of a ListMessages read that
	// answers 429 Retry-After: 30.
	Throttle map[string]int
	// Headers / Attachments by Graph message id.
	Headers     map[string][]graph.Header
	Attachments map[string][]graph.Attachment
	// Sent captures sendMail calls (answered 202).
	Sent []SentMail
	// HeaderCalls counts MessageHeaders requests.
	HeaderCalls int
	// ThrottleHeaders makes every MessageHeaders call answer 429 Retry-After: 30.
	ThrottleHeaders bool
}

// NewMail returns an empty fake mail server and a client pointed at it.
func NewMail(t testing.TB) (*MailFake, *Server, *graph.Client) {
	t.Helper()
	f := &MailFake{
		Boxes: map[string]map[string][]graph.Message{}, Forbidden: map[string]bool{}, Throttle: map[string]int{},
		Headers: map[string][]graph.Header{}, Attachments: map[string][]graph.Attachment{},
	}
	srv, c := New(t, nil)
	srv.API = f.Handler(func() string { return srv.URL })
	return f, srv, c
}

// Add stores messages under their Mailbox/Folder.
func (f *MailFake) Add(msgs ...graph.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		if f.Boxes[m.Mailbox] == nil {
			f.Boxes[m.Mailbox] = map[string][]graph.Message{}
		}
		f.Boxes[m.Mailbox][m.Folder] = append(f.Boxes[m.Mailbox][m.Folder], m)
	}
}

// SentCount returns the number of captured sends.
func (f *MailFake) SentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Sent)
}

// Handler serves the fake; base returns the server URL (for nextLinks).
func (f *MailFake) Handler(base func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		mb, err := url.PathUnescape(Mailbox(r))
		if err != nil || mb == "" {
			http.NotFound(w, r)
			return
		}
		if f.Forbidden[mb] {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`))
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v1.0/users/"+Mailbox(r))
		switch {
		case r.Method == http.MethodPost && rest == "/sendMail":
			raw, _ := io.ReadAll(r.Body)
			var p map[string]any
			_ = json.Unmarshal(raw, &p)
			f.Sent = append(f.Sent, SentMail{Sender: mb, Payload: p})
			w.WriteHeader(http.StatusAccepted)
		case strings.HasPrefix(rest, "/mailFolders/"):
			folder := strings.TrimSuffix(strings.TrimPrefix(rest, "/mailFolders/"), "/messages")
			f.list(w, r, base(), mb, folder)
		case strings.HasSuffix(rest, "/attachments"):
			id, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(rest, "/messages/"), "/attachments"))
			var vals []map[string]any
			for _, a := range f.Attachments[id] {
				vals = append(vals, map[string]any{"@odata.type": "#microsoft.graph.fileAttachment", "id": a.ID,
					"name": a.Name, "contentType": a.ContentType, "size": a.Size, "isInline": false})
			}
			writeJSON(w, map[string]any{"value": nonNil(vals)})
		case strings.HasPrefix(rest, "/messages/"):
			id, _ := url.PathUnescape(strings.TrimPrefix(rest, "/messages/"))
			f.HeaderCalls++
			if f.ThrottleHeaders {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			var hs []map[string]string
			for _, h := range f.Headers[id] {
				hs = append(hs, map[string]string{"name": h.Name, "value": h.Value})
			}
			writeJSON(w, map[string]any{"id": id, "internetMessageHeaders": nonNil(hs)})
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *MailFake) list(w http.ResponseWriter, r *http.Request, base, mb, folder string) {
	q := r.URL.Query()
	since := time.Time{}
	if v, ok := strings.CutPrefix(q.Get("$filter"), "receivedDateTime ge "); ok {
		since, _ = time.Parse(time.RFC3339, v)
	}
	top, _ := strconv.Atoi(q.Get("$top"))
	if top <= 0 {
		top = 50
	}
	if top == 1 { // ProbeRead
		writeJSON(w, map[string]any{"value": []any{}})
		return
	}
	offset, _ := strconv.Atoi(q.Get("$skiptoken"))
	if page, ok := f.Throttle[mb]; ok && page == offset/top {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	var match []graph.Message
	for _, m := range f.Boxes[mb][folder] {
		if !m.ReceivedAt.Before(since) {
			match = append(match, m)
		}
	}
	slices.SortStableFunc(match, func(a, b graph.Message) int { return a.ReceivedAt.Compare(b.ReceivedAt) })
	end := min(offset+top, len(match))
	var vals []map[string]any
	for _, m := range match[min(offset, len(match)):end] {
		vals = append(vals, toWire(m))
	}
	resp := map[string]any{"value": nonNil(vals)}
	if end < len(match) {
		q.Set("$skiptoken", strconv.Itoa(end))
		resp["@odata.nextLink"] = base + r.URL.Path + "?" + q.Encode()
	}
	writeJSON(w, resp)
}

func toWire(m graph.Message) map[string]any {
	addr := func(a string) map[string]any { return map[string]any{"emailAddress": map[string]string{"address": a}} }
	var to, cc []map[string]any
	for _, a := range m.To {
		to = append(to, addr(a))
	}
	for _, a := range m.CC {
		cc = append(cc, addr(a))
	}
	return map[string]any{
		"id": m.ID, "internetMessageId": m.InternetMessageID, "conversationId": m.ConversationID,
		"subject":      m.Subject,
		"from":         map[string]any{"emailAddress": map[string]string{"address": m.From, "name": m.FromName}},
		"toRecipients": nonNil(to), "ccRecipients": nonNil(cc),
		"sentDateTime": m.SentAt.UTC().Format(time.RFC3339), "receivedDateTime": m.ReceivedAt.UTC().Format(time.RFC3339),
		"hasAttachments": m.HasAttachments,
		"body":           map[string]string{"contentType": "text", "content": m.BodyText},
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
