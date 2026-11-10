package graph

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/GoGPUtils/stringutil"
)

const (
	defaultTop      = 50
	defaultMaxPages = 20
	previewLen      = 255
	// messageSelect: bccRecipients deliberately omitted (nothing consumes it).
	messageSelect = "id,internetMessageId,conversationId,subject,from,sender,toRecipients,ccRecipients,sentDateTime,receivedDateTime,hasAttachments,body"
)

// Message is one normalized mail. Addresses are lower-cased. IsAuto, IsBulk
// and IsList are not set by ListMessages: the classify job fills them from
// the message headers.
type Message struct {
	ID, InternetMessageID, ConversationID string
	Mailbox, Folder                       string
	Subject, From, FromName               string
	BodyText, BodyPreview                 string
	To, CC                                []string
	ReceivedAt, SentAt                    time.Time
	HasAttachments                        bool
	IsAuto, IsBulk, IsList                bool
}

// ListOptions bounds a ListMessages call. Truncated, when set, is called with
// the number of messages fetched when the page cap stopped pagination.
type ListOptions struct {
	Top, MaxPages int
	Truncated     func(fetched int)
}

type emailAddress struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type wireMessage struct {
	ID                string         `json:"id"`
	InternetMessageID string         `json:"internetMessageId"`
	ConversationID    string         `json:"conversationId"`
	Subject           string         `json:"subject"`
	From              emailAddress   `json:"from"`
	Sender            emailAddress   `json:"sender"`
	To                []emailAddress `json:"toRecipients"`
	CC                []emailAddress `json:"ccRecipients"`
	SentDateTime      time.Time      `json:"sentDateTime"`
	ReceivedDateTime  time.Time      `json:"receivedDateTime"`
	HasAttachments    bool           `json:"hasAttachments"`
	Body              struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
}

// ListMessages reads one folder (inbox or sentitems) of one mailbox, newest
// first, for messages received at or after since. It follows @odata.nextLink
// up to opts.MaxPages; hitting the cap calls opts.Truncated (never silent).
func (c *Client) ListMessages(ctx context.Context, mailbox, folder string, since time.Time, opts ListOptions) ([]Message, error) {
	if opts.Top <= 0 {
		opts.Top = defaultTop
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = defaultMaxPages
	}
	q := url.Values{}
	q.Set("$filter", "receivedDateTime ge "+since.UTC().Format(time.RFC3339))
	q.Set("$orderby", "receivedDateTime desc")
	q.Set("$top", strconv.Itoa(opts.Top))
	q.Set("$select", messageSelect)
	next := c.baseURL + "/users/" + url.PathEscape(mailbox) + "/mailFolders/" + url.PathEscape(folder) + "/messages?" + q.Encode()
	headers := map[string]string{"Prefer": `outlook.body-content-type="text"`}

	var out []Message
	for page := 0; next != ""; page++ {
		if page == opts.MaxPages {
			if opts.Truncated != nil {
				opts.Truncated(len(out))
			}
			break
		}
		if !strings.HasPrefix(next, c.baseURL+"/") {
			return nil, fmt.Errorf("graph: refusing nextLink outside %s", c.baseURL)
		}
		var resp struct {
			NextLink string        `json:"@odata.nextLink"`
			Value    []wireMessage `json:"value"`
		}
		if err := c.getJSON(ctx, next, mailbox, headers, &resp); err != nil {
			return nil, err
		}
		for _, w := range resp.Value {
			out = append(out, w.normalize(mailbox, folder))
		}
		next = resp.NextLink
	}
	return out, nil
}

func (w wireMessage) normalize(mailbox, folder string) Message {
	from := w.From
	if from.EmailAddress.Address == "" {
		from = w.Sender
	}
	body := w.Body.Content
	if strings.EqualFold(w.Body.ContentType, "html") {
		body = htmlToText(body)
	} else {
		body = strings.TrimSpace(body)
	}
	return Message{
		ID:                w.ID,
		InternetMessageID: w.InternetMessageID,
		ConversationID:    w.ConversationID,
		Mailbox:           mailbox,
		Folder:            folder,
		Subject:           w.Subject,
		From:              strings.ToLower(from.EmailAddress.Address),
		FromName:          from.EmailAddress.Name,
		BodyText:          body,
		BodyPreview:       stringutil.Truncate(stringutil.NormalizeWhitespace(body), previewLen, "…"),
		To:                addresses(w.To),
		CC:                addresses(w.CC),
		ReceivedAt:        w.ReceivedDateTime.UTC(),
		SentAt:            w.SentDateTime.UTC(),
		HasAttachments:    w.HasAttachments,
	}
}

func addresses(in []emailAddress) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		if a.EmailAddress.Address != "" {
			out = append(out, strings.ToLower(a.EmailAddress.Address))
		}
	}
	return out
}

// htmlToText is a minimal fallback (the Prefer header normally makes Graph
// return text): drop tags, decode entities, collapse whitespace.
func htmlToText(s string) string {
	s = strings.NewReplacer("<br>", " ", "<br/>", " ", "<br />", " ", "</p>", " </p>", "</div>", " </div>").Replace(s)
	return stringutil.NormalizeWhitespace(stringutil.StripHTMLEntities(stringutil.StripTags(s)))
}
