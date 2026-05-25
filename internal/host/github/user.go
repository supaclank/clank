package github

import (
	"context"
	"errors"
	"fmt"

	gogithub "github.com/google/go-github/v66/github"
)

// getAuthenticatedUser fetches the calling token's login + numeric ID
// via GET /user. Called from the device-flow goroutine immediately
// after a successful token exchange so the credential file is written
// with the login already populated — that lets the status endpoint
// render "@login" without a follow-up API call on every refresh.
func (m *Manager) getAuthenticatedUser(ctx context.Context, accessToken string) (login string, userID int64, err error) {
	c, err := m.apiClient(accessToken)
	if err != nil {
		return "", 0, fmt.Errorf("build api client: %w", err)
	}
	u, _, err := c.Users.Get(ctx, "")
	if err != nil {
		var er *gogithub.ErrorResponse
		if errors.As(err, &er) {
			return "", 0, fmt.Errorf("get user: github http %d: %s", er.Response.StatusCode, er.Message)
		}
		return "", 0, fmt.Errorf("get user: %w", err)
	}
	return u.GetLogin(), u.GetID(), nil
}
