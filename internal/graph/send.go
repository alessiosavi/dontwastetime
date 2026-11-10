package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// SendMail sends one HTML mail from sender's mailbox. It is the only write dwt
// performs (digest and P0 alerts). Graph answers 202 Accepted; any other
// answer is an error.
func (c *Client) SendMail(ctx context.Context, sender string, to []string, subject, html string, saveToSentItems bool) error {
	if sender == "" || len(to) == 0 {
		return errors.New("graph: sendMail needs a sender and at least one recipient")
	}
	type addr struct {
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	}
	rcpts := make([]addr, len(to))
	for i, a := range to {
		rcpts[i].EmailAddress.Address = a
	}
	payload := map[string]any{
		"message": map[string]any{
			"subject":      subject,
			"body":         map[string]string{"contentType": "HTML", "content": html},
			"toRecipients": rcpts,
		},
		"saveToSentItems": saveToSentItems,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("graph: encode sendMail: %w", err)
	}
	resp, err := c.do(ctx, request{
		method:  http.MethodPost,
		url:     c.baseURL + "/users/" + url.PathEscape(sender) + "/sendMail",
		body:    body,
		mailbox: sender,
	})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("graph: sendMail: unexpected HTTP %d (want 202)", resp.StatusCode)
	}
	return nil
}
