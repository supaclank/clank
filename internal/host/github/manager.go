package github

import (
	"context"
	"net/http"
	"time"
)

// ClientIDEnv is the environment variable carrying the Clank GitHub
// OAuth App's client_id. When unset, the Manager reports
// available:false and refuses to start the device flow. The env-var
// approach lets sprite provisioners set it without code changes; the
// laptop's clankd forwards it from its own environment.
const ClientIDEnv = "CLANK_GITHUB_OAUTH_CLIENT_ID"

// Manager owns this host's GitHub integration: the credential store,
// the in-progress device flow (if any), and the HTTP client used for
// outbound calls to github.com. Single Manager per host.Service.
//
// Device-flow plumbing lands in PR 2; PR creation plumbing lands in
// PR 3. PR 1 wires up the Store + status surface only.
type Manager struct {
	store    *Store
	httpc    *http.Client
	clientID string
}

// NewManager constructs a Manager rooted at homeDir with the given
// OAuth App client_id. Empty clientID is allowed — the manager will
// report available:false and refuse to start the device flow, which
// is the correct behavior for hosts that haven't been configured
// with a GitHub integration (e.g. self-hosters who didn't set the
// env var, or laptop dev runs without one).
func NewManager(homeDir, clientID string) *Manager {
	return &Manager{
		store:    NewStore(homeDir),
		httpc:    &http.Client{Timeout: 30 * time.Second},
		clientID: clientID,
	}
}

// SetHTTPClient overrides the client used for outbound GitHub calls.
// Tests use this to point at an httptest.Server simulating github.com.
func (m *Manager) SetHTTPClient(c *http.Client) {
	if c != nil {
		m.httpc = c
	}
}

// Store returns the credential store. Exposed for PRs 2/3 that need
// to write/read credentials from the device-flow goroutine and the
// PR creation path.
func (m *Manager) Store() *Store { return m.store }

// HTTPClient returns the configured client. PRs 2/3 use this for
// outbound GitHub calls so tests can swap it via SetHTTPClient.
func (m *Manager) HTTPClient() *http.Client { return m.httpc }

// ClientID returns the configured OAuth App client_id; empty when
// the manager is unavailable.
func (m *Manager) ClientID() string { return m.clientID }

// IsAvailable reports whether GitHub Connect is enabled on this host.
// Returns false when ClientIDEnv was unset at startup.
func (m *Manager) IsAvailable() bool { return m.clientID != "" }

// Status is the wire shape returned from GET /credentials/github/status.
// `available` is the host-level capability flag; `connected` is the
// per-user "do we have a token" flag. Both are needed by the UI so it
// can distinguish "this host doesn't support GitHub" from "this host
// supports it but you haven't connected yet."
type Status struct {
	Available   bool      `json:"available"`
	Connected   bool      `json:"connected"`
	GitHubLogin string    `json:"github_login,omitempty"`
	Scopes      []string  `json:"scopes,omitempty"`
	InstalledAt time.Time `json:"installed_at,omitzero"`
}

// Status returns the current connection state. Reads the credential
// file on every call — cheap and avoids stale-cache bugs across
// disconnect/reconnect.
func (m *Manager) Status(_ context.Context) (Status, error) {
	c, err := m.store.Read()
	if err != nil {
		return Status{}, err
	}
	return Status{
		Available:   m.IsAvailable(),
		Connected:   c.AccessToken != "",
		GitHubLogin: c.GitHubLogin,
		Scopes:      c.Scopes,
		InstalledAt: c.InstalledAt,
	}, nil
}

// Disconnect removes the stored credential. Idempotent: missing file
// is not an error.
func (m *Manager) Disconnect(_ context.Context) error {
	return m.store.Delete()
}
