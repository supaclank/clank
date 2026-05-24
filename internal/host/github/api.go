package github

// HTTP plumbing for outbound calls to github.com (OAuth endpoints)
// and api.github.com (user info, PR creation). Base URLs are
// overridable so tests can point at an httptest.Server. The Manager
// owns one *http.Client and two base URLs; helpers in this file and
// in user.go / pr.go construct requests against them.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Default base URLs. Overridable per-Manager via SetAuthBaseURL /
// SetAPIBaseURL so tests can swap in an httptest.Server.
const (
	defaultAuthBaseURL = "https://github.com"
	defaultAPIBaseURL  = "https://api.github.com"
)

// userAgent is sent on every outbound request. GitHub requires a
// User-Agent header for most endpoints.
const userAgent = "clank/host"

// SetAuthBaseURL overrides the github.com base URL used for the
// device-flow and token-exchange calls. Tests set this to
// httptest.Server.URL; production leaves it at the default.
func (m *Manager) SetAuthBaseURL(u string) {
	if u != "" {
		m.authBaseURL = u
	}
}

// SetAPIBaseURL overrides the api.github.com base URL for user info
// and (PR 3) pull-request creation.
func (m *Manager) SetAPIBaseURL(u string) {
	if u != "" {
		m.apiBaseURL = u
	}
}

// doJSON sends req, decodes a 200 JSON body into out, and surfaces
// non-2xx responses as errors carrying the server-side message.
// Caller sets headers and body — this is just the round-trip and
// decode.
func (m *Manager) doJSON(req *http.Request, out any) error {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// Surface transport truncation as a clear read error rather
		// than letting json.Unmarshal complain about a partial body.
		return fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// HTTPError carries a non-2xx response from GitHub up to callers so
// they can map specific status codes to typed errors (e.g. 401 → token
// invalid, 422 → already exists).
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("github http %d: %s", e.Status, e.Body)
}
