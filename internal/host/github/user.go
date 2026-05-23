package github

import (
	"context"
	"fmt"
	"net/http"
)

// userResp is the subset of GET /user we care about. GitHub returns
// many more fields; we ignore them. Login is the @handle the UI
// shows; ID is the stable numeric identifier we persist so we can
// distinguish "user renamed" from "different user" in future flows.
type userResp struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// getAuthenticatedUser fetches the calling token's user info. Called
// from the device-flow goroutine immediately after a successful token
// exchange so the credential file is written with the login already
// populated — that lets the status endpoint render "@login" without
// a follow-up API call on every refresh.
func (m *Manager) getAuthenticatedUser(ctx context.Context, accessToken string) (login string, userID int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.apiBaseURL+"/user", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	var out userResp
	if err := m.doJSON(req, &out); err != nil {
		return "", 0, fmt.Errorf("get user: %w", err)
	}
	return out.Login, out.ID, nil
}
