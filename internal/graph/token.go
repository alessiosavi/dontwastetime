package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	tokenKey    = "app"
	tokenMargin = 60 * time.Second
	graphScope  = "https://graph.microsoft.com/.default"
)

type tokenEntry struct {
	value     string
	expiresAt time.Time // already reduced by tokenMargin
}

// token returns a cached app-only token, fetching a new one when absent or
// within the 60s safety margin. Concurrent misses share one fetch.
func (c *Client) token(ctx context.Context) (string, error) {
	for range 2 {
		e, err := c.tokens.GetOrLoad(ctx, tokenKey, c.fetchToken)
		if err != nil {
			return "", err
		}
		if c.now().Before(e.expiresAt) {
			return e.value, nil
		}
		c.tokens.Delete(tokenKey)
	}
	return "", fmt.Errorf("graph: token endpoint returned an already-expired token")
}

func (c *Client) fetchToken(ctx context.Context) (tokenEntry, error) {
	form := url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"scope":         {graphScope},
		"grant_type":    {"client_credentials"},
	}
	endpoint := c.authURL + "/" + url.PathEscape(c.tenantID) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenEntry{}, fmt.Errorf("graph: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return tokenEntry{}, fmt.Errorf("graph: token request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return tokenEntry{}, fmt.Errorf("graph: token response: %w", err)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		// Only the status and the OAuth error code: the response never
		// echoes the secret, but we do not print it either way.
		return tokenEntry{}, fmt.Errorf("graph: token request failed: HTTP %d %s", resp.StatusCode, body.Error)
	}
	return tokenEntry{
		value:     body.AccessToken,
		expiresAt: c.now().Add(time.Duration(body.ExpiresIn)*time.Second - tokenMargin),
	}, nil
}
