package hostclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/acksell/clank/internal/agent"
)

// SessionsClient is the collection-scoped handle for session operations
// that don't target a single existing session. Obtained via
// HTTP.Sessions().
type SessionsClient struct {
	c *HTTP
}

// Sessions returns the collection-scoped session handle.
func (c *HTTP) Sessions() *SessionsClient {
	return &SessionsClient{c: c}
}

// Create starts a new session on the host. Returns a SessionBackend
// adapter bound to the new session and the host-resolved server URL
// (empty string for backends without an HTTP server, e.g. Claude Code).
func (s *SessionsClient) Create(ctx context.Context, sessionID string, req agent.StartRequest) (agent.SessionBackend, string, error) {
	body := struct {
		SessionID string             `json:"session_id"`
		Request   agent.StartRequest `json:"request"`
	}{sessionID, req}
	var snap struct {
		SessionID  string              `json:"session_id"`
		ExternalID string              `json:"external_id"`
		Status     agent.SessionStatus `json:"status"`
		ServerURL  string              `json:"server_url"`
	}
	if err := s.c.do(ctx, http.MethodPost, "/sessions", body, &snap); err != nil {
		return nil, "", err
	}
	// Bind the backend to the id the host actually returned. The host
	// is the source of truth: if it allocated a different id (or none
	// at all) we must not silently keep operating against the caller's
	// assumed id, which would target a nonexistent session.
	if snap.SessionID == "" {
		return nil, "", fmt.Errorf("hostclient: server returned empty session id (requested %q)", sessionID)
	}
	if sessionID != "" && snap.SessionID != sessionID {
		return nil, "", fmt.Errorf("hostclient: server returned session id %q, expected %q", snap.SessionID, sessionID)
	}
	return newHTTPSessionBackend(s.c, snap.SessionID, snap.ExternalID, snap.Status), snap.ServerURL, nil
}
