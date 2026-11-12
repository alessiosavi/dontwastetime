package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ErrDeliveryUnknown means the sendMail request may have reached Graph but
// delivery cannot be confirmed (no answer or an unexpected success status):
// the mail may have been delivered, so a retry could send it twice.
var ErrDeliveryUnknown = errors.New("graph: sendMail delivery not confirmed; the mail may have been delivered")

// SendMail sends one HTML mail from sender's mailbox. It is the only write dwt
// performs (digest, P0 alerts and send-note). Graph answers 202 Accepted; no
// answer or another 2xx wraps ErrDeliveryUnknown, any other answer is an error.
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
	if err := ctx.Err(); err != nil {
		return err // cancelled before sending: definitely not delivered
	}
	resp, err := c.do(ctx, request{
		method:  http.MethodPost,
		url:     c.baseURL + "/users/" + url.PathEscape(sender) + "/sendMail",
		body:    body,
		mailbox: sender,
	})
	if err != nil {
		if na := (*noAnswerError)(nil); errors.As(err, &na) && !na.neverSent() {
			return fmt.Errorf("%w: %w", ErrDeliveryUnknown, err)
		}
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("graph: sendMail: unexpected HTTP %d (want 202): %w", resp.StatusCode, ErrDeliveryUnknown)
	}
	return nil
}
