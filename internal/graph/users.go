package graph

import (
	"context"
	"net/url"
	"strings"
)

// User is a directory user as returned by /users (needs User.Read.All).
type User struct {
	ID, Mail, UserPrincipalName string
	AccountEnabled              bool
}

// ListUsers pages through /users. A 403 wraps ErrForbidden: the credential
// lacks User.Read.All, which only gates the optional discovery helper.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	next := c.baseURL + "/users?" + url.Values{
		"$select": {"id,mail,userPrincipalName,accountEnabled"},
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
				AccountEnabled    bool   `json:"accountEnabled"`
			} `json:"value"`
		}
		if err := c.getJSON(ctx, next, "", nil, &resp); err != nil {
			return nil, err
		}
		for _, u := range resp.Value {
			out = append(out, User{ID: u.ID, Mail: u.Mail, UserPrincipalName: u.UserPrincipalName, AccountEnabled: u.AccountEnabled})
		}
		next = resp.NextLink
	}
	return out, nil
}
