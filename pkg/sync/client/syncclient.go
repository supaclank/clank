// Package syncclient is the laptop side of clank-sync. It registers
// worktrees and pushes checkpoint bundles via the gateway-orchestrated
// migration substrate. See checkpoints.go for the public methods.
package syncclient

import (
	"fmt"
	"net/http"
	"time"
)

// defaultResponseHeaderTimeout caps how long we'll wait for response
// headers on presign / upload / download calls. Bundle bodies can
// legitimately stream for much longer, so we don't cap Client.Timeout
// (which would also kill the body). Callers thread a context.Context
// for end-to-end deadlines.
const defaultResponseHeaderTimeout = 30 * time.Second

// defaultHTTPClient builds an http.Client whose Transport is cloned
// from http.DefaultTransport so we keep ProxyFromEnvironment,
// IdleConnTimeout, TLSHandshakeTimeout etc., and adds a
// ResponseHeaderTimeout so a stuck server doesn't hang the CLI/TUI.
func defaultHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	return &http.Client{Transport: t}
}

// Config configures a Client.
type Config struct {
	// BaseURL is the clank-sync endpoint, e.g. "https://sync.example.com".
	BaseURL string

	// AuthToken is sent as `Authorization: Bearer <token>` on every
	// upload. Required for non-permissive deployments.
	AuthToken string

	// HTTPClient overrides the default http.Client. Optional.
	HTTPClient *http.Client
}

// Client uploads bundles to a clank-sync server.
type Client struct {
	cfg    Config
	client *http.Client
}

// New constructs a Client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("syncclient: BaseURL is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = defaultHTTPClient()
	}
	return &Client{cfg: cfg, client: cfg.HTTPClient}, nil
}
