// Package gateway is the daemon's single ingress: it authenticates,
// resolves the user to a persistent host via the provisioner, and
// reverse-proxies everything else through.
//
// Routing: /ping and /gateway/health are served locally; every other
// path proxies to the user's host with the provisioner-supplied
// transport injecting per-request auth.
package gateway

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// AuthConfig is the public OAuth 2.0 discovery payload returned by
// GET /auth-config. Embedders populate Config.AuthConfig with their
// IdP details; the gateway serves it via AuthConfigHandler. Daemons
// must mount that handler PRE-auth (it's the laptop's bootstrap
// route — clank has no token when it calls it).
//
// Standard OAuth 2.0 only — Supabase OAuth Server, Auth0, Keycloak,
// Okta, etc. all fit this shape. Nothing provider-specific.
type AuthConfig struct {
	AuthorizeEndpoint string   `json:"authorize_endpoint"`
	TokenEndpoint     string   `json:"token_endpoint"`
	ClientID          string   `json:"client_id"`
	Scopes            []string `json:"scopes,omitempty"`
	DefaultProvider   string   `json:"default_provider,omitempty"`

	// CallbackPort, when set, instructs the laptop to bind its
	// PKCE callback listener to exactly this port. Required when
	// the IdP matches redirect_uris strictly (e.g. Supabase OAuth
	// Server). The IdP must have `http://127.0.0.1:<port>` in its
	// redirect_uris allow-list. Zero = random kernel-assigned port
	// (RFC 8252 default for native apps).
	CallbackPort int `json:"callback_port,omitempty"`
}

// Config wires the gateway's dependencies. Provisioner is required;
// Sync is optional (when nil, the migration route returns 503 and
// the /v1/ prefix isn't mounted).
//
// Authentication is the responsibility of an outer middleware (see
// pkg/auth.Middleware) — by the time a request reaches the gateway,
// the verified Principal is already in r.Context().
type Config struct {
	// Provisioner resolves a userID into the user's HostRef. EnsureHost
	// is called per-request; the provisioner caches in-process.
	Provisioner provisioner.Provisioner

	// Sync is the embedded sync server. When non-nil, the gateway mounts
	// the sync API routes under /v1/ and the migration route calls sync
	// methods directly rather than via HTTP. When nil, the migration
	// route returns 503.
	Sync *clanksync.Server

	// OwnerCache holds the laptop daemon's cached view of which
	// worktrees the active remote owns. When non-nil AND Sync == nil
	// (laptop mode), the gateway mounts the /sessions* router that
	// proxies per-session ops to the active remote for remote-owned
	// worktrees. When nil, /sessions/* falls through to today's
	// proxyToHost (the catch-all). The cloud gateway (Sync != nil)
	// never has an OwnerCache — it is the destination of the proxy,
	// not the source.
	OwnerCache *OwnerCache

	// RemoteResolver provides the active remote's URL+JWT for the
	// /sessions* router's outbound calls. Required iff OwnerCache is
	// set; same supplier as the OwnerCache itself, but threaded
	// separately so the router can call out without sharing state.
	RemoteResolver RemoteResolver

	// AuthConfig, when non-nil, makes AuthConfigHandler() return a
	// handler that serves this payload as JSON. Daemons wire that
	// handler pre-auth on GET /auth-config so the laptop can
	// discover the IdP before it has a token.
	AuthConfig *AuthConfig
}

// Gateway is the public ingress.
type Gateway struct {
	cfg Config
	log *log.Logger

	// migrationKey signs two-phase migration tokens. Random-on-startup
	// so a daemon restart invalidates any pending materialize → commit
	// in flight; the laptop re-runs `clank pull --migrate`.
	migrationKey []byte

	// ownerCache is a convenience handle on cfg.OwnerCache so the
	// per-session routing helpers don't have to spell out cfg.OwnerCache
	// at every call site.
	ownerCache *OwnerCache
}

// NewGateway constructs a Gateway.
func NewGateway(cfg Config, lg *log.Logger) (*Gateway, error) {
	if cfg.Provisioner == nil {
		return nil, fmt.Errorf("gateway: Provisioner is required")
	}
	if cfg.OwnerCache != nil && cfg.RemoteResolver == nil {
		return nil, fmt.Errorf("gateway: OwnerCache requires RemoteResolver")
	}
	if cfg.OwnerCache != nil && cfg.Sync != nil {
		return nil, fmt.Errorf("gateway: OwnerCache is only valid in laptop mode (Sync must be nil)")
	}
	if lg == nil {
		lg = log.Default()
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("gateway: generate migration signing key: %w", err)
	}
	return &Gateway{cfg: cfg, log: lg, migrationKey: key, ownerCache: cfg.OwnerCache}, nil
}

// Handler returns the public-listener http.Handler.
//
// /ping and /gateway/health answer locally without waking a host;
// /v1/migrate/worktrees/{id} runs the gateway-orchestrated migration
// flow when Sync is configured; /v1/ (other paths) forwards to the
// embedded sync server when Sync is configured; every other path
// proxies to the user's host. Authentication is handled by an outer
// middleware (pkg/auth.Middleware); handlers read the Principal from
// r.Context() via auth.MustPrincipal.
func (g *Gateway) Handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("GET /ping", g.handlePing)
	mx.HandleFunc("GET /gateway/health", g.handleGatewayHealth)
	mx.HandleFunc("POST /v1/migrate/worktrees/{id}", g.handleMigrateWorktree)
	mx.HandleFunc("POST /v1/migrate/worktrees/{id}/materialize", g.handleMigrateMaterialize)
	mx.HandleFunc("POST /v1/migrate/worktrees/{id}/commit", g.handleMigrateCommit)
	if g.cfg.Sync != nil {
		// POST /v1/migrate/worktrees/{id} is more specific and wins
		// over the /v1/ prefix registered here.
		mx.Handle("/v1/", g.cfg.Sync.Handler())
	}
	if g.ownerCache != nil {
		// Laptop mode: per-session routing decides local-vs-remote
		// based on worktree ownership. /sessions/search is mounted
		// explicitly so /sessions/{id} below doesn't match "search"
		// as a session id.
		mx.HandleFunc("GET /sessions", g.handleListSessions)
		mx.HandleFunc("GET /sessions/search", g.handleSearchSessions)
		mx.HandleFunc("POST /sessions", g.handleCreateSession)
		mx.HandleFunc("/sessions/{id}", g.handlePerSession)
		mx.HandleFunc("/sessions/{id}/", g.handlePerSession)
	}
	mx.HandleFunc("/", g.proxyToHost)
	return mx
}

func (g *Gateway) handlePing(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("pong\n"))
}

func (g *Gateway) handleGatewayHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// AuthConfigHandler returns an http.Handler that serves the
// configured AuthConfig as JSON, or nil when AuthConfig is unset.
// Daemons must mount this PRE-auth (GET /auth-config is the laptop's
// bootstrap discovery route — clank has no token yet at that point).
//
// Returning a nil handler when AuthConfig is unset lets callers wire
// the route conditionally without ceremony — `if h := gw.AuthConfigHandler();
// h != nil { mux.Handle("GET /auth-config", h) }`.
func (g *Gateway) AuthConfigHandler() http.Handler {
	if g.cfg.AuthConfig == nil {
		return nil
	}
	// Pre-encode once; the payload doesn't change at runtime.
	body, _ := json.Marshal(g.cfg.AuthConfig)
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(body)
	})
}
