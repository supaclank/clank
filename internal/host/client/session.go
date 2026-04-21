package hostclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// SessionClient is a per-session handle. Obtained via HTTP.Session(id).
//
// Most per-session operations live on the SessionBackend returned by
// Sessions().Create — the SessionClient handle is for operations the
// hub initiates against a session id without holding the backend
// (currently just Stop).
type SessionClient struct {
	c  *HTTP
	id string
}

// Session returns a handle for the session with the given hub-side id.
func (c *HTTP) Session(id string) *SessionClient {
	return &SessionClient{c: c, id: id}
}

// errEmptySessionID is returned when an operation is attempted on a
// SessionClient bound to an empty id. We fail fast rather than letting
// the request hit "/sessions//stop" and produce a confusing 404.
var errEmptySessionID = errors.New("session id is required")

// path builds a URL path with the session id properly escaped so that
// ids containing reserved characters (slash, question mark, hash, …)
// or empty ids cannot produce malformed routes.
func (s *SessionClient) path(suffix string) (string, error) {
	if s.id == "" {
		return "", errEmptySessionID
	}
	return "/sessions/" + url.PathEscape(s.id) + suffix, nil
}

// Stop releases the session on the host.
func (s *SessionClient) Stop(ctx context.Context) error {
	p, err := s.path("/stop")
	if err != nil {
		return err
	}
	return s.c.do(ctx, http.MethodPost, p, nil, nil)
}
