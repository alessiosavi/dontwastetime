// Package graphtest provides an in-process fake Microsoft Graph for tests in
// other packages: it serves the token endpoint and routes API calls to a
// handler. Import it from _test.go files only.
package graphtest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/graph"
)

// Clock is the fixed instant the fake's client uses for token expiry.
var Clock = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

// Server is a fake Graph host. API may be replaced between calls.
type Server struct {
	URL      string
	API      http.HandlerFunc
	APICalls atomic.Int32
}

// New starts a fake and returns it with a client pointed at it.
func New(t testing.TB, api http.HandlerFunc) (*Server, *graph.Client) {
	t.Helper()
	f := &Server{API: api}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fake","expires_in":3600}`))
			return
		}
		f.APICalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		f.API(w, r)
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	c := graph.New("tenant", "client", "secret",
		graph.WithBaseURL(srv.URL+"/v1.0"), graph.WithAuthURL(srv.URL),
		graph.WithClock(func() time.Time { return Clock }))
	return f, c
}

// Mailbox extracts the mailbox segment from a /v1.0/users/{mailbox}/... path.
func Mailbox(r *http.Request) string {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1.0/users/")
	if !ok {
		return ""
	}
	mb, _, _ := strings.Cut(rest, "/")
	return mb
}
