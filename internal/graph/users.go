package graph

import (
	"context"
	"net/url"
	"strings"
)

// User is a directory user as returned by /users. Mail is empty when the
// credential's directory permission does not expose it.
type User struct {
	ID, Mail, UserPrincipalName string
}

// ListUsers pages through /users. A 403 wraps ErrForbidden: the credential
// cannot list users, which only gates the optional discovery helper.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	next := c.baseURL + "/users?" + url.Values{
		"$select": {"id,mail,userPrincipalName"},
		"$top":    {"999"},
	}.Encode()
	var out []User
	for page := 0; next != "" && page < defaultMaxPages; page++ {
		if !strings.HasPrefix(next, c.baseURL+"/") {
			return nil, errForeignNextLink(c.baseURL)
		}
		var resp struct {
			NextLink string `json:"@odata.nextLink"`
			Value    []struct {
				ID                string `json:"id"`
				Mail              string `json:"mail"`
				UserPrincipalName string `json:"userPrincipalName"`
			} `json:"value"`
		}
		if err := c.getJSON(ctx, next, "", nil, &resp); err != nil {
			return nil, err
		}
		for _, u := range resp.Value {
			out = append(out, User{ID: u.ID, Mail: u.Mail, UserPrincipalName: u.UserPrincipalName})
		}
		next = resp.NextLink
	}
	return out, nil
}
