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

// Create starts a new session on the host. The host generates the
// session ID. Returns a SessionBackend adapter bound to the new session
// and the host-resolved server URL (empty string for backends without
// an HTTP server, e.g. Claude Code).
func (s *SessionsClient) Create(ctx context.Context, req agent.StartRequest) (agent.SessionBackend, string, error) {
	var info agent.SessionInfo
	if err := s.c.do(ctx, http.MethodPost, "/sessions", req, &info); err != nil {
		return nil, "", err
	}
	if info.ID == "" {
		return nil, "", fmt.Errorf("hostclient: server returned empty session id")
	}
	return newHTTPSessionBackend(s.c, info.ID, info.ExternalID, info.Status), info.ServerURL, nil
}
