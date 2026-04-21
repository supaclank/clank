package hubclient

import (
	"context"
	"net/url"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// SessionsClient is the collection-level handle for sessions.
type SessionsClient struct {
	c *Client
}

// Sessions returns the collection-level sessions handle.
func (c *Client) Sessions() *SessionsClient {
	return &SessionsClient{c: c}
}

// Create asks clankd to create and start a new agent session.
func (s *SessionsClient) Create(ctx context.Context, req agent.StartRequest) (*agent.SessionInfo, error) {
	var info agent.SessionInfo
	if err := s.c.post(ctx, "/sessions", req, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// List returns all managed sessions.
func (s *SessionsClient) List(ctx context.Context) ([]agent.SessionInfo, error) {
	var sessions []agent.SessionInfo
	if err := s.c.get(ctx, "/sessions", &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// Search searches session metadata. See agent.SearchParams for the query
// semantics.
func (s *SessionsClient) Search(ctx context.Context, p agent.SearchParams) ([]agent.SessionInfo, error) {
	v := url.Values{}
	if p.Query != "" {
		v.Set("q", p.Query)
	}
	if !p.Since.IsZero() {
		v.Set("since", p.Since.Format(time.RFC3339))
	}
	if !p.Until.IsZero() {
		v.Set("until", p.Until.Format(time.RFC3339))
	}
	if p.Visibility != "" {
		v.Set("visibility", string(p.Visibility))
	}
	var sessions []agent.SessionInfo
	if err := s.c.get(ctx, "/sessions/search?"+v.Encode(), &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// Discover asks clankd to discover and register historical sessions for
// the given project directory.
func (s *SessionsClient) Discover(ctx context.Context, projectDir string) error {
	body := struct {
		ProjectDir string `json:"project_dir"`
	}{ProjectDir: projectDir}
	return s.c.post(ctx, "/sessions/discover", body, nil)
}

// Subscribe opens an SSE stream and delivers events to the returned
// channel. The channel closes when the context is cancelled or the
// connection drops.
func (s *SessionsClient) Subscribe(ctx context.Context) (<-chan agent.Event, error) {
	resp, err := s.c.openSSE(ctx, "/events")
	if err != nil {
		return nil, err
	}
	ch := make(chan agent.Event, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		parseSSEStream(resp.Body, ch)
	}()
	return ch, nil
}
