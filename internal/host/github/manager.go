package github

import (
	"context"
	"net/http"
	"sync"
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
// Device-flow plumbing lives in device_flow.go; PR creation lives in
// pr.go (PR 3). Base URLs are overridable via SetAuthBaseURL /
// SetAPIBaseURL so tests can swap an httptest.Server.
type Manager struct {
	store       *Store
	httpc       *http.Client
	clientID    string
	authBaseURL string
	apiBaseURL  string

	// pollSafetyMargin is added to the interval GitHub returns,
	// defending against clock skew. Tests set this to 0 so the flow
	// runs at the GitHub-returned interval — the fake server returns
	// 1s, total polling cadence becomes 1s instead of 4s.
	pollSafetyMargin time.Duration

	// flowMu guards currentFlow. Single-slot registry: only one
	// device flow may be in-flight per host at a time. A second
	// StartConnect cancels the previous flow before starting.
	flowMu      sync.Mutex
	currentFlow *flowState

	// ghCLIFallback lets AccessToken borrow the machine's own gh CLI
	// login when the store holds no token. Set once at wiring time via
	// EnableGhCLIFallback, before the manager serves requests.
	ghCLIFallback bool
}

// NewManager constructs a Manager rooted at homeDir with the given
// OAuth App client_id. Empty clientID is allowed — the manager will
// report available:false and refuse to start the device flow, which
// is the correct behavior for hosts that haven't been configured
// with a GitHub integration (e.g. self-hosters who didn't set the
// env var, or laptop dev runs without one).
func NewManager(homeDir, clientID string) *Manager {
	return &Manager{
		store: NewStore(homeDir),
		httpc: &http.Client{
			Timeout: 30 * time.Second,
			// Inject our User-Agent on every outbound request — GitHub
			// requires it on most endpoints, and both oauth2 and
			// go-github reach through this client.
			Transport: &userAgentTransport{base: http.DefaultTransport},
		},
		clientID:         clientID,
		authBaseURL:      defaultAuthBaseURL,
		apiBaseURL:       defaultAPIBaseURL,
		pollSafetyMargin: defaultPollSafetyMargin,
	}
}

// userAgentTransport sets a fixed User-Agent on every outbound
// request. Required because oauth2 and go-github both use whatever
// http.Client we hand them; neither sets a UA themselves.
type userAgentTransport struct {
	base http.RoundTripper
}

func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") != "" {
		return t.base.RoundTrip(req)
	}
	// net/http.RoundTripper forbids mutating the caller's request —
	// clone before injecting our header.
	cloned := req.Clone(req.Context())
	cloned.Header.Set("User-Agent", userAgent)
	return t.base.RoundTrip(cloned)
}

// EnableGhCLIFallback lets AccessToken (and Status) fall back to the
// machine's own gh CLI login when no clank connection exists. A
// deployment decision, not a default: the local laptop provisioner
// enables it — the host IS the user's machine, where agent sessions
// could run `gh auth token` themselves, so borrowing it exposes
// nothing new — while sandboxes keep token access explicit. Call once
// at wiring time, before the manager serves requests.
func (m *Manager) EnableGhCLIFallback() { m.ghCLIFallback = true }

// SetPollSafetyMargin overrides the slack added to each device-flow
// poll interval. Defaults to 3s (matches RFC 8628 §3.4 guidance);
// tests set 0 so the flow polls at GitHub's returned cadence.
func (m *Manager) SetPollSafetyMargin(d time.Duration) {
	if d >= 0 {
		m.pollSafetyMargin = d
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
// CredentialSource values for Status.Source.
const (
	// CredentialSourceStore is a token from clank's own device-flow
	// store — connected/disconnected through clank.
	CredentialSourceStore = "store"
	// CredentialSourceGhCLI is a token borrowed live from the
	// machine's gh CLI login. clank stores nothing and cannot
	// disconnect it — that's `gh auth logout`'s job — so clients
	// should hide the disconnect affordance for this source.
	CredentialSourceGhCLI = "gh_cli"
)

// `available` is the host-level capability flag; `connected` is the
// per-user "do we have a token" flag. Both are needed by the UI so it
// can distinguish "this host doesn't support GitHub" from "this host
// supports it but you haven't connected yet."
type Status struct {
	Available   bool      `json:"available"`
	Connected   bool      `json:"connected"`
	Source      string    `json:"source,omitempty"` // CredentialSource* when connected
	GitHubLogin string    `json:"github_login,omitempty"`
	Scopes      []string  `json:"scopes,omitempty"`
	InstalledAt time.Time `json:"installed_at,omitzero"`
}

// Status returns the current connection state. Reads the credential
// file on every call — cheap and avoids stale-cache bugs across
// disconnect/reconnect. With the gh CLI fallback enabled, a host with
// no stored token but a logged-in gh still reports connected (source
// gh_cli, login unknown — identity isn't part of gh's token handoff).
func (m *Manager) Status(_ context.Context) (Status, error) {
	c, err := m.store.Read()
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Available:   m.IsAvailable(),
		Connected:   c.AccessToken != "",
		GitHubLogin: c.GitHubLogin,
		Scopes:      c.Scopes,
		InstalledAt: c.InstalledAt,
	}
	if st.Connected {
		st.Source = CredentialSourceStore
	} else if m.ghCLIFallback && ghCLIToken() != "" {
		st.Connected = true
		st.Source = CredentialSourceGhCLI
	}
	return st, nil
}

// Disconnect removes the stored credential. Idempotent: missing file
// is not an error.
func (m *Manager) Disconnect(_ context.Context) error {
	return m.store.Delete()
}
