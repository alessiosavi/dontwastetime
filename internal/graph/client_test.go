package graph

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeGraph serves the token endpoint itself and delegates Graph paths to api.
type fakeGraph struct {
	srv        *httptest.Server
	tokenCalls atomic.Int32
	apiCalls   atomic.Int32
	api        http.HandlerFunc
}

func newFake(t *testing.T, api http.HandlerFunc) (*fakeGraph, *Client, *fakeClock) {
	t.Helper()
	f := &fakeGraph{api: api}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
			n := f.tokenCalls.Add(1)
			if r.FormValue("grant_type") != "client_credentials" || r.FormValue("client_secret") != "secret" {
				http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok-` + string(rune('0'+n)) + `","expires_in":3600,"token_type":"Bearer"}`))
			return
		}
		f.apiCalls.Add(1)
		f.api(w, r)
	}))
	t.Cleanup(f.srv.Close)
	clk := &fakeClock{t: t0}
	c := New("tenant", "client", "secret",
		WithBaseURL(f.srv.URL+"/v1.0"), WithAuthURL(f.srv.URL), WithClock(clk.Now))
	return f, c, clk
}

func fixture(t *testing.T, name, base string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.ReplaceAll(string(b), "{{BASE}}", base))
}

// pagedAPI serves page1, then page2 when $skiptoken=page2 is requested.
func pagedAPI(t *testing.T, base func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("$skiptoken") == "page2" {
			_, _ = w.Write(fixture(t, "messages_page2.json", base()))
			return
		}
		_, _ = w.Write(fixture(t, "messages_page1.json", base()))
	}
}

var since = time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)

func TestToken_Cached(t *testing.T) {
	var f *fakeGraph
	f, c, clk := newFake(t, nil)
	f.api = pagedAPI(t, func() string { return f.srv.URL })
	ctx := context.Background()
	for range 2 {
		if _, err := c.ListMessages(ctx, "marketing@example.test", "inbox", since, ListOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("token requests after two calls = %d, want 1", n)
	}
	clk.Advance(3600*time.Second - 61*time.Second)
	if _, err := c.ListMessages(ctx, "marketing@example.test", "inbox", since, ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("token refreshed before the 60s margin: %d requests", n)
	}
	clk.Advance(2 * time.Second) // now inside the 60s safety margin
	if _, err := c.ListMessages(ctx, "marketing@example.test", "inbox", since, ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := f.tokenCalls.Load(); n != 2 {
		t.Fatalf("token not refreshed inside the margin: %d requests", n)
	}
}

func TestListMessages_Paginates(t *testing.T) {
	var f *fakeGraph
	var first *http.Request
	f, c, _ := newFake(t, nil)
	inner := pagedAPI(t, func() string { return f.srv.URL })
	f.api = func(w http.ResponseWriter, r *http.Request) {
		if first == nil {
			first = r.Clone(context.Background())
		}
		inner(w, r)
	}
	msgs, err := c.ListMessages(context.Background(), "marketing@example.test", "inbox", since, ListOptions{Top: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].ID != "AAMk-1" || msgs[1].ID != "AAMk-2" || msgs[2].ID != "AAMk-3" {
		t.Fatalf("pages not parsed in order: %+v", msgs)
	}
	q := first.URL.Query()
	sel := q.Get("$select")
	if !strings.Contains(sel, "body") || strings.Contains(sel, "bccRecipients") {
		t.Errorf("$select = %q: must include body, never bccRecipients", sel)
	}
	if got := q.Get("$filter"); got != "receivedDateTime ge 2026-11-08T00:00:00Z" {
		t.Errorf("$filter = %q", got)
	}
	if got := q.Get("$orderby"); got != "receivedDateTime desc" {
		t.Errorf("$orderby = %q", got)
	}
	if got := q.Get("$top"); got != "2" {
		t.Errorf("$top = %q", got)
	}
	if got := first.Header.Get("Authorization"); got != "Bearer tok-1" {
		t.Errorf("Authorization = %q", got)
	}
	if !strings.Contains(first.Header.Get("Prefer"), `outlook.body-content-type="text"`) {
		t.Errorf("Prefer header = %q", first.Header.Get("Prefer"))
	}
	if !strings.Contains(first.URL.EscapedPath(), "/users/marketing@example.test/mailFolders/inbox/messages") {
		t.Errorf("path = %q", first.URL.EscapedPath())
	}
	m := msgs[0]
	if m.Mailbox != "marketing@example.test" || m.Folder != "inbox" || m.From != "mario.sample@client.example" || m.FromName != "Mario Sample" {
		t.Errorf("identity fields: %+v", m)
	}
	if m.InternetMessageID != "<m1@client.example>" || m.ConversationID != "conv-1" || m.Subject != "Rinnovo contratto" {
		t.Errorf("ids/subject: %+v", m)
	}
	if len(m.To) != 1 || m.To[0] != "marketing@example.test" || len(m.CC) != 1 || m.CC[0] != "ops@example.test" {
		t.Errorf("recipients: to=%v cc=%v", m.To, m.CC)
	}
	if !m.ReceivedAt.Equal(time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)) || !m.SentAt.Equal(time.Date(2026, 11, 9, 8, 59, 0, 0, time.UTC)) {
		t.Errorf("times: %v %v", m.ReceivedAt, m.SentAt)
	}
	if m.BodyText != "Buongiorno, serve il rinnovo entro venerdì." || m.BodyPreview == "" {
		t.Errorf("body: %q preview %q", m.BodyText, m.BodyPreview)
	}
	if got := msgs[1].BodyText; got != "Ciao & benvenuti alla newsletter" {
		t.Errorf("html body not stripped to text: %q", got)
	}
	if !msgs[1].HasAttachments || msgs[0].IsAuto || msgs[0].IsBulk || msgs[0].IsList {
		t.Errorf("flags: %+v", msgs[1])
	}
}

func TestListMessages_TruncationSignal(t *testing.T) {
	var f *fakeGraph
	f, c, _ := newFake(t, nil)
	f.api = pagedAPI(t, func() string { return f.srv.URL })
	truncatedAt := -1
	msgs, err := c.ListMessages(context.Background(), "marketing@example.test", "inbox", since,
		ListOptions{MaxPages: 1, Truncated: func(n int) { truncatedAt = n }})
	if err != nil {
		t.Fatal(err)
	}
	if truncatedAt != 2 || len(msgs) != 2 {
		t.Fatalf("truncated signal = %d, msgs = %d; want 2, 2", truncatedAt, len(msgs))
	}
}

func TestListMessages_RejectsForeignNextLink(t *testing.T) {
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"@odata.nextLink":"https://evil.example/steal","value":[]}`))
	})
	if _, err := c.ListMessages(context.Background(), "m@example.test", "inbox", since, ListOptions{}); err == nil {
		t.Fatal("nextLink on a foreign host must be refused (token would leak)")
	}
}

func TestListMessages_401RetriesOnce(t *testing.T) {
	var calls atomic.Int32
	f, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"value":[]}`))
	})
	if _, err := c.ListMessages(context.Background(), "m@example.test", "inbox", since, ListOptions{}); err != nil {
		t.Fatalf("retry after one 401 should succeed: %v", err)
	}
	if f.tokenCalls.Load() != 2 {
		t.Errorf("token cache not invalidated on 401: %d token requests", f.tokenCalls.Load())
	}

	f2, c2, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	_, err := c2.ListMessages(context.Background(), "m@example.test", "inbox", since, ListOptions{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("two consecutive 401s: want ErrUnauthorized, got %v", err)
	}
	if n := f2.apiCalls.Load(); n != 2 {
		t.Fatalf("401 retry loop: %d API calls, want exactly 2", n)
	}
}

func TestListMessages_403Typed(t *testing.T) {
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`))
	})
	_, err := c.ListMessages(context.Background(), "legal@example.test", "inbox", since, ListOptions{})
	var fe *ErrMailboxForbidden
	if !errors.As(err, &fe) || fe.Mailbox != "legal@example.test" {
		t.Fatalf("want *ErrMailboxForbidden for legal@, got %v", err)
	}
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "access policy") {
		t.Errorf("forbidden error must wrap ErrForbidden and hint at the access policy: %v", err)
	}
}

func TestListMessages_429Typed(t *testing.T) {
	_, c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.ListMessages(context.Background(), "m@example.test", "inbox", since, ListOptions{})
	var te *ErrThrottled
	if !errors.As(err, &te) || te.RetryAfter != 30*time.Second {
		t.Fatalf("want *ErrThrottled{30s}, got %v", err)
	}
}
