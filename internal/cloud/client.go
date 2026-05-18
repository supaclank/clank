// Package cloud is the laptop's HTTP client for the user's clank
// gateway deployment. Provider-agnostic: the gateway exposes an
// /auth-config discovery endpoint that returns standard OAuth 2.0
// endpoints (authorize, token, client_id, scopes), and clank's OAuth
// client (oauth.go) runs authorization code + PKCE against them.
//
// Designed for the TUI's Cloud panel (internal/tui/cloudview.go) and
// the `clank login` subcommand. Every call is one-shot, Context-bounded.
// No background goroutines, no caching: callers own lifecycle.
package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnauthorized is returned when the bearer is rejected by the
// gateway (e.g. token expired). Caller should re-prompt sign-in.
var ErrUnauthorized = errors.New("cloud: unauthorized")

// Client wraps an *http.Client targeting the gateway base URL. It
// covers the small bootstrap surface the laptop needs *before* it
// has a session — currently just /auth-config. Authenticated calls
// (sync, sessions, etc.) go through dedicated clients elsewhere
// (e.g. pkg/sync/client) wrapping the same gateway URL.
type Client struct {
	gatewayURL string
	http       *http.Client
}

// New constructs a Client targeting the gateway base URL (e.g.
// "https://clankgw.fly.dev"). httpClient may be nil for the default.
func New(gatewayURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{gatewayURL: strings.TrimRight(gatewayURL, "/"), http: httpClient}
}

// AuthConfig is the gateway's reply to GET /auth-config. It tells
// clank which OAuth 2.0 IdP to talk to. The endpoint is public (no
// auth) because clank has no token yet at the point it calls it.
//
// All fields are standard OAuth 2.0 — Supabase OAuth Server, Auth0,
// Okta, Keycloak, etc. all populate the same shape.
type AuthConfig struct {
	// AuthorizeEndpoint is the IdP's /authorize URL, e.g.
	// "https://abc.supabase.co/oauth/authorize".
	AuthorizeEndpoint string `json:"authorize_endpoint"`

	// TokenEndpoint is the IdP's /token URL, e.g.
	// "https://abc.supabase.co/oauth/token".
	TokenEndpoint string `json:"token_endpoint"`

	// ClientID is the public OAuth client identifier the laptop
	// presents at both endpoints. PKCE replaces the client secret;
	// nothing secret is shipped to the laptop.
	ClientID string `json:"client_id"`

	// Scopes are the OAuth scopes the laptop should request. Joined
	// with spaces on the authorize URL per RFC 6749 §3.3.
	Scopes []string `json:"scopes,omitempty"`

	// DefaultProvider is an optional IdP hint (e.g. "github") the
	// gateway suggests as the primary sign-in option. Passed as the
	// non-spec `provider` query param when set; ignored by IdPs that
	// don't recognise it.
	DefaultProvider string `json:"default_provider,omitempty"`

	// CallbackPort, when set, tells the laptop to bind its PKCE
	// callback listener to exactly this port (instead of a random
	// kernel-assigned port). Required by IdPs that match
	// redirect_uris strictly — Supabase OAuth Server, for example.
	// The IdP must have the same `http://127.0.0.1:<port>` registered
	// as a redirect_uri for the OAuth client.
	CallbackPort int `json:"callback_port,omitempty"`
}

// FetchAuthConfig calls GET <gateway>/auth-config to discover the
// IdP details. Public endpoint — no Authorization header.
func (c *Client) FetchAuthConfig(ctx context.Context) (*AuthConfig, error) {
	url := c.gatewayURL + "/auth-config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch auth-config: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth-config: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var cfg AuthConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("parse auth-config: %w", err)
	}
	if cfg.AuthorizeEndpoint == "" || cfg.TokenEndpoint == "" || cfg.ClientID == "" {
		return nil, fmt.Errorf("auth-config: response missing authorize_endpoint, token_endpoint, or client_id")
	}
	if cfg.CallbackPort < 0 || cfg.CallbackPort > 65535 {
		return nil, fmt.Errorf("auth-config: callback_port %d out of range", cfg.CallbackPort)
	}
	return &cfg, nil
}

// Session is the credential set returned after a successful OAuth
// grant. ExpiresAt is unix-seconds; treat AccessToken as invalid
// once time.Now().Unix() > ExpiresAt.
type Session struct {
	AccessToken  string
	RefreshToken string
	UserID       string
	UserEmail    string
	ExpiresAt    int64
}

// GatewayURL returns the configured gateway base URL. Useful for
// callers that need to construct sub-paths (e.g. sync clients).
func (c *Client) GatewayURL() string { return c.gatewayURL }
