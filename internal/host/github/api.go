package github

// Builders for the two SDK clients we use outbound:
//   - golang.org/x/oauth2 for the device flow (against github.com)
//   - github.com/google/go-github for the REST API (api.github.com)
//
// Both honor base URLs overridable via SetAuthBaseURL / SetAPIBaseURL
// so tests can swap in an httptest.Server. The Manager owns one
// *http.Client and feeds it into both SDKs.

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	gogithub "github.com/google/go-github/v66/github"
	"golang.org/x/oauth2"
)

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
// and PR creation.
func (m *Manager) SetAPIBaseURL(u string) {
	if u != "" {
		m.apiBaseURL = u
	}
}

// oauth2Config builds the per-Manager oauth2.Config. Endpoint URLs
// derive from m.authBaseURL so tests pointing at httptest.Server keep
// working unchanged.
func (m *Manager) oauth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID: m.clientID,
		Scopes:   requestedScopes(),
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: m.authBaseURL + "/login/device/code",
			TokenURL:      m.authBaseURL + "/login/oauth/access_token",
			// GitHub's device flow has no client_secret; we want
			// client_id in the body, not a probe-retry against Basic
			// auth that swallows a polling response on every error.
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

// oauth2Context wraps ctx so the oauth2 package uses m.httpc for the
// device-flow calls. oauth2 reads the client out of context via the
// oauth2.HTTPClient key — see internal.ContextClient.
func (m *Manager) oauth2Context(parent context.Context) context.Context {
	return context.WithValue(parent, oauth2.HTTPClient, m.httpc)
}

// apiClient builds an authenticated go-github client pointed at
// m.apiBaseURL. Returns an error only when m.apiBaseURL is set to
// something url.Parse rejects (test misconfiguration).
func (m *Manager) apiClient(token string) (*gogithub.Client, error) {
	c := gogithub.NewClient(m.httpc)
	if token != "" {
		c = c.WithAuthToken(token)
	}
	c.UserAgent = userAgent
	if m.apiBaseURL == defaultAPIBaseURL {
		return c, nil
	}
	// go-github concatenates path segments onto BaseURL, so a trailing
	// slash is required. httptest.Server URLs don't carry one.
	base := m.apiBaseURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse api base url: %w", err)
	}
	c.BaseURL = u
	return c, nil
}
