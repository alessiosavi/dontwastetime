package graph

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Header is one internet message header.
type Header struct{ Name, Value string }

// Attachment is file-attachment metadata only; content is never fetched in v1.
type Attachment struct {
	ID, Name, ContentType string
	Size                  int64
}

func (c *Client) messageURL(mailbox, messageID string) string {
	return c.baseURL + "/users/" + url.PathEscape(mailbox) + "/messages/" + url.PathEscape(messageID)
}

// MessageHeaders fetches the internet headers of one message (one call; used
// only for inbound classify candidates).
func (c *Client) MessageHeaders(ctx context.Context, mailbox, messageID string) ([]Header, error) {
	var resp struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"internetMessageHeaders"`
	}
	u := c.messageURL(mailbox, messageID) + "?" + url.Values{"$select": {"internetMessageHeaders"}}.Encode()
	if err := c.getJSON(ctx, u, mailbox, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]Header, 0, len(resp.Headers))
	for _, h := range resp.Headers {
		out = append(out, Header{Name: h.Name, Value: h.Value})
	}
	return out, nil
}

// ListAttachments returns metadata of the non-inline file attachments of one
// message. Item/reference attachments and inline images are skipped.
func (c *Client) ListAttachments(ctx context.Context, mailbox, messageID string) ([]Attachment, error) {
	next := c.messageURL(mailbox, messageID) + "/attachments?" +
		url.Values{"$select": {"id,name,contentType,size,isInline"}}.Encode()
	var out []Attachment
	for page := 0; next != "" && page < defaultMaxPages; page++ {
		var resp struct {
			NextLink string `json:"@odata.nextLink"`
			Value    []struct {
				Type        string `json:"@odata.type"`
				ID          string `json:"id"`
				Name        string `json:"name"`
				ContentType string `json:"contentType"`
				Size        int64  `json:"size"`
				IsInline    bool   `json:"isInline"`
			} `json:"value"`
		}
		if err := c.getJSON(ctx, next, mailbox, nil, &resp); err != nil {
			return nil, err
		}
		for _, a := range resp.Value {
			if a.Type != "#microsoft.graph.fileAttachment" || a.IsInline {
				continue
			}
			out = append(out, Attachment{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size})
		}
		next = resp.NextLink
		if next != "" && !strings.HasPrefix(next, c.baseURL+"/") {
			return nil, errForeignNextLink(c.baseURL)
		}
	}
	return out, nil
}

// InboxID returns the id of the mailbox's Inbox folder. It is a cheap proof
// that the credential can read the mailbox and a stable identity: an alias
// and the primary address of one mailbox return the same id. Errors are typed
// as ListMessages (403 → *ErrMailboxForbidden, 404 → *HTTPError).
func (c *Client) InboxID(ctx context.Context, mailbox string) (string, error) {
	u := c.baseURL + "/users/" + url.PathEscape(mailbox) + "/mailFolders/inbox?" + url.Values{"$select": {"id"}}.Encode()
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.getJSON(ctx, u, mailbox, nil, &resp); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", fmt.Errorf("graph: inbox of %s: empty id", mailbox)
	}
	return resp.ID, nil
}

// ProbeRead reads at most one message id from a folder: the cheapest proof
// that the credential can read the mailbox. Errors are typed as ListMessages.
func (c *Client) ProbeRead(ctx context.Context, mailbox, folder string) error {
	u := c.baseURL + "/users/" + url.PathEscape(mailbox) + "/mailFolders/" + url.PathEscape(folder) +
		"/messages?" + url.Values{"$top": {"1"}, "$select": {"id"}}.Encode()
	var resp struct{}
	return c.getJSON(ctx, u, mailbox, nil, &resp)
}
