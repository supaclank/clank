package hubclient

import (
	"context"
	"errors"
	"net/url"

	"github.com/acksell/clank/internal/agent"
)

// errEmptySessionID is returned when a SessionClient is constructed
// without a non-empty id. We fail fast rather than issue requests
// against /sessions/ which would silently target a different endpoint.
var errEmptySessionID = errors.New("hubclient: empty session id")

// SessionClient is bound to a session ID. All id-scoped session
// operations live here, including Get (which materialises the handle
// into the underlying SessionInfo).
type SessionClient struct {
	c  *Client
	id string
}

// Session returns a handle for the given session id.
func (c *Client) Session(id string) *SessionClient {
	return &SessionClient{c: c, id: id}
}

// ID returns the bound session id.
func (s *SessionClient) ID() string { return s.id }

// path builds a /sessions/<id><suffix> URL with the id properly escaped
// so that ids containing reserved characters cannot break out of the
// session scope. Callers receive an error when the bound id is empty.
func (s *SessionClient) path(suffix string) (string, error) {
	if s.id == "" {
		return "", errEmptySessionID
	}
	return "/sessions/" + url.PathEscape(s.id) + suffix, nil
}

// Get returns the SessionInfo for this session.
func (s *SessionClient) Get(ctx context.Context) (*agent.SessionInfo, error) {
	p, err := s.path("")
	if err != nil {
		return nil, err
	}
	var info agent.SessionInfo
	if err := s.c.get(ctx, p, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Messages returns the full message history for this session.
func (s *SessionClient) Messages(ctx context.Context) ([]agent.MessageData, error) {
	p, err := s.path("/messages")
	if err != nil {
		return nil, err
	}
	var messages []agent.MessageData
	if err := s.c.get(ctx, p, &messages); err != nil {
		return nil, err
	}
	return messages, nil
}

// Send sends a follow-up message to the running session.
func (s *SessionClient) Send(ctx context.Context, opts agent.SendMessageOpts) error {
	p, err := s.path("/message")
	if err != nil {
		return err
	}
	return s.c.post(ctx, p, opts, nil)
}

// Abort interrupts the running session.
func (s *SessionClient) Abort(ctx context.Context) error {
	p, err := s.path("/abort")
	if err != nil {
		return err
	}
	return s.c.post(ctx, p, nil, nil)
}

// Revert reverts the session to messageID, removing all subsequent messages.
func (s *SessionClient) Revert(ctx context.Context, messageID string) error {
	p, err := s.path("/revert")
	if err != nil {
		return err
	}
	body := struct {
		MessageID string `json:"message_id"`
	}{MessageID: messageID}
	return s.c.post(ctx, p, body, nil)
}

// Fork forks the session from messageID. Empty messageID forks the entire
// session (from start).
func (s *SessionClient) Fork(ctx context.Context, messageID string) (*agent.SessionInfo, error) {
	p, err := s.path("/fork")
	if err != nil {
		return nil, err
	}
	body := struct {
		MessageID string `json:"message_id,omitempty"`
	}{MessageID: messageID}
	var info agent.SessionInfo
	if err := s.c.post(ctx, p, body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// MarkRead marks the session as read.
func (s *SessionClient) MarkRead(ctx context.Context) error {
	p, err := s.path("/read")
	if err != nil {
		return err
	}
	return s.c.post(ctx, p, nil, nil)
}

// ToggleFollowUp toggles the follow-up flag and returns the new state.
func (s *SessionClient) ToggleFollowUp(ctx context.Context) (bool, error) {
	p, err := s.path("/followup")
	if err != nil {
		return false, err
	}
	var resp struct {
		FollowUp bool `json:"follow_up"`
	}
	if err := s.c.post(ctx, p, nil, &resp); err != nil {
		return false, err
	}
	return resp.FollowUp, nil
}

// SetVisibility sets the visibility state of the session.
func (s *SessionClient) SetVisibility(ctx context.Context, visibility agent.SessionVisibility) error {
	p, err := s.path("/visibility")
	if err != nil {
		return err
	}
	body := struct {
		Visibility agent.SessionVisibility `json:"visibility"`
	}{Visibility: visibility}
	return s.c.post(ctx, p, body, nil)
}

// SetDraft sets or clears the draft text for the session.
func (s *SessionClient) SetDraft(ctx context.Context, draft string) error {
	p, err := s.path("/draft")
	if err != nil {
		return err
	}
	body := struct {
		Draft string `json:"draft"`
	}{Draft: draft}
	return s.c.post(ctx, p, body, nil)
}

// Delete stops and removes the session.
func (s *SessionClient) Delete(ctx context.Context) error {
	p, err := s.path("")
	if err != nil {
		return err
	}
	return s.c.do(ctx, "DELETE", p, nil, nil)
}

// ReplyPermission replies to a permission request.
func (s *SessionClient) ReplyPermission(ctx context.Context, permissionID string, allow bool) error {
	if permissionID == "" {
		return errors.New("hubclient: empty permission id")
	}
	p, err := s.path("/permissions/" + url.PathEscape(permissionID) + "/reply")
	if err != nil {
		return err
	}
	body := map[string]bool{"allow": allow}
	return s.c.post(ctx, p, body, nil)
}

// PendingPermissions returns all pending permissions for the session.
func (s *SessionClient) PendingPermissions(ctx context.Context) ([]agent.PermissionData, error) {
	p, err := s.path("/pending-permission")
	if err != nil {
		return nil, err
	}
	var perms []agent.PermissionData
	if err := s.c.get(ctx, p, &perms); err != nil {
		return nil, err
	}
	return perms, nil
}
