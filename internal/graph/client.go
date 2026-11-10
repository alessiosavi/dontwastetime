// Package graph is a minimal, read-mostly Microsoft Graph client: app-only
// client_credentials auth, typed errors, paginated mail reads, and the single
// write dwt is allowed (sendMail for the digest and P0 alerts).
//
// Logs and errors carry IDs, mailboxes and status codes only — never tokens,
// secrets or message bodies.
package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/GoGPUtils/cache"
)

const (
	defaultBaseURL = "https://graph.microsoft.com/v1.0"
	defaultAuthURL = "https://login.microsoftonline.com"
	// maxErrorBody bounds how much of an error response is read.
	maxErrorBody = 64 << 10
)

// ErrUnauthorized is returned when Graph answers 401 twice in a row (one
// retry after invalidating the cached token, never a loop).
var ErrUnauthorized = errors.New("graph: unauthorized (401 after token refresh)")

// ErrForbidden is the base of every 403. Mailbox-scoped calls return
// *ErrMailboxForbidden, which wraps it.
var ErrForbidden = errors.New("graph: forbidden (403)")

// ErrMailboxForbidden means the credential cannot read this mailbox.
type ErrMailboxForbidden struct{ Mailbox string }

func (e *ErrMailboxForbidden) Error() string {
	return fmt.Sprintf("graph: mailbox %s forbidden (403): outside the application access policy?", e.Mailbox)
}

func (e *ErrMailboxForbidden) Unwrap() error { return ErrForbidden }

// ErrThrottled is a 429; RetryAfter comes from the Retry-After header.
type ErrThrottled struct{ RetryAfter time.Duration }

func (e *ErrThrottled) Error() string {
	return fmt.Sprintf("graph: throttled (429), retry after %s", e.RetryAfter)
}

// HTTPError is any other non-success Graph answer. Code/Message are Graph's
// error code and message, never request or response bodies.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("graph: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

// Client is safe for concurrent use.
type Client struct {
	tenantID, clientID, clientSecret string
	baseURL, authURL                 string
	http                             *http.Client
	now                              func() time.Time
	tokens                           *cache.Cache[string, tokenEntry]
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the Graph API root (default https://graph.microsoft.com/v1.0).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithAuthURL overrides the identity platform root (default https://login.microsoftonline.com).
func WithAuthURL(u string) Option { return func(c *Client) { c.authURL = strings.TrimRight(u, "/") } }

// WithClock sets the clock used for token expiry. Required: the wall clock is
// read only in cmd/dwt, which passes it in.
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// WithHTTPClient overrides the HTTP client (default: 60s timeout).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// New builds a client for one app registration.
func New(tenantID, clientID, clientSecret string, opts ...Option) *Client {
	tokens, err := cache.New(cache.Config[string, tokenEntry]{MaxEntries: 1})
	if err != nil { // static config; cannot fail
		panic(err)
	}
	c := &Client{
		tenantID: tenantID, clientID: clientID, clientSecret: clientSecret,
		baseURL: defaultBaseURL, authURL: defaultAuthURL,
		http:   &http.Client{Timeout: 60 * time.Second},
		tokens: tokens,
		now:    func() time.Time { panic("graph: WithClock is required") },
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// request describes one Graph call; body is re-created for the 401 retry.
type request struct {
	method  string
	url     string // absolute
	body    []byte
	headers map[string]string
	mailbox string // non-empty: 403 maps to *ErrMailboxForbidden
}

// do performs req with a bearer token, retrying exactly once on 401 after
// invalidating the cached token, and maps failures to typed errors. On
// success the caller owns resp.Body.
func (c *Client) do(ctx context.Context, req request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		tok, err := c.token(ctx)
		if err != nil {
			return nil, err
		}
		var body io.Reader
		if req.body != nil {
			body = strings.NewReader(string(req.body))
		}
		hr, err := http.NewRequestWithContext(ctx, req.method, req.url, body)
		if err != nil {
			return nil, fmt.Errorf("graph: build request: %w", err)
		}
		hr.Header.Set("Authorization", "Bearer "+tok)
		hr.Header.Set("Accept", "application/json")
		if req.body != nil {
			hr.Header.Set("Content-Type", "application/json")
		}
		for k, v := range req.headers {
			hr.Header.Set(k, v)
		}
		resp, err := c.http.Do(hr)
		if err != nil {
			return nil, fmt.Errorf("graph: %s %s: %w", req.method, redactURL(req.url), err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			c.tokens.Delete(tokenKey)
			if attempt == 0 {
				continue
			}
			return nil, ErrUnauthorized
		case http.StatusForbidden:
			if req.mailbox != "" {
				return nil, &ErrMailboxForbidden{Mailbox: req.mailbox}
			}
			return nil, fmt.Errorf("%w: %s", ErrForbidden, graphErr(resp.StatusCode, raw).Code)
		case http.StatusTooManyRequests:
			return nil, &ErrThrottled{RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
		default:
			return nil, graphErr(resp.StatusCode, raw)
		}
	}
}

// getJSON GETs an absolute URL and decodes the JSON answer into out.
func (c *Client) getJSON(ctx context.Context, url, mailbox string, headers map[string]string, out any) error {
	resp, err := c.do(ctx, request{method: http.MethodGet, url: url, mailbox: mailbox, headers: headers})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("graph: decode %s: %w", redactURL(url), err)
	}
	return nil
}

func graphErr(status int, raw []byte) *HTTPError {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	return &HTTPError{Status: status, Code: env.Error.Code, Message: env.Error.Message}
}

// retryAfter parses a Retry-After seconds value; unknown → 30s (Graph's
// usual hint), never zero.
func retryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 30 * time.Second
}

// redactURL drops the query string (it can carry skip tokens) from errors.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
