// Package gateway is the daemon's single ingress: it authenticates,
// resolves the user to a persistent host via the provisioner, and
// reverse-proxies everything else through.
//
// Routing: /ping and /gateway/health are served locally; every other
// path proxies to the user's host with the provisioner-supplied
// transport injecting per-request auth.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/notify"
	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/tokens"
	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
	clanksync "github.com/acksell/clank/pkg/sync"
)

// PreviewHostLookup is the narrow surface preview handlers need from
// the hoststore: resolve a sprite's per-host notifier bearer to the
// owning host row (and through it the user_id). Same shape as
// notify.HostLookup; defined here to keep gateway from importing the
// notify package just for an interface alias.
//
// hoststore.HostStore satisfies this; cloud embedders wire their
// pgx-backed implementation into both notify and preview.
type PreviewHostLookup interface {
	GetHostByNotifierToken(ctx context.Context, notifierToken string) (hoststore.Host, error)
}

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

	// Notify, when non-nil, exposes mobile push notifications:
	//   - /devices (POST, DELETE) mount inside Handler() and inherit
	//     whatever user-auth middleware wraps it (same model as Sync).
	//   - /webhooks/notifications mounts pre-auth via the daemon's
	//     parent mux — see NotifyWebhookHandler. The dispatcher does
	//     its own host-bearer verification, so wrapping it with user
	//     auth would reject the host call outright.
	//
	// Construct the dispatcher with notify.NewDispatcher; the laptop
	// daemon passes its in-process store, cloud embedders pass their
	// pgx-backed implementations. Either way, the gateway just mounts.
	Notify *notify.Dispatcher

	// PreviewRoutes is the persistence for tokenized preview URLs. When
	// set together with PreviewHostLookup and PreviewRootDomain, the
	// gateway mounts:
	//   - /v1/preview/tokens/{token}/share, DELETE /v1/preview/tokens/{token},
	//     GET /v1/preview/tokens — owner-facing token management. Inherits
	//     the outer auth wrap.
	//   - PreviewWebhookHandler() — sprite-facing register/revoke,
	//     analogous to NotifyWebhookHandler. Mounted pre-auth by the
	//     daemon.
	// All three must be set together or none — leaving any nil
	// disables the entire preview surface.
	PreviewRoutes routestore.Store

	// PreviewHostLookup resolves a sprite's notifier_token bearer to
	// the host row, used to authenticate /webhooks/preview/*. Same
	// pattern as notify.Dispatcher's HostLookup.
	PreviewHostLookup PreviewHostLookup

	// PreviewRootDomain is the wildcard zone preview URLs live under,
	// e.g. "clankexample.dev". Combined with the per-token leftmost label
	// (preview-<token>) by pkg/preview/tokens.HostFor. Required to
	// render the URL that the register webhook returns to the sprite.
	PreviewRootDomain string

	// PreviewAuthenticator verifies JWTs on owner-only preview-URL
	// requests. Same Authenticator the daemon's main auth.Middleware
	// uses (clank passes its OIDC verifier here). The subdomain
	// proxy lives OUTSIDE auth.Middleware because public-visibility
	// tokens must accept anonymous requests — so we run Verify inline
	// for owner-only tokens only.
	//
	// Required when PreviewRoutes is set.
	PreviewAuthenticator auth.Authenticator

	// PreviewSigningKey is the HMAC secret used to sign short-lived
	// owner-only preview URLs. Clients that can't carry an
	// Authorization header (Expo's dev-launcher, the RN bundle
	// runtime) authenticate via a `?clank_sig=…&clank_exp=…` bearer
	// minted by POST /v1/preview/tokens/{token}/sign.
	//
	// Required when PreviewRoutes is set. Must be at least
	// tokens.MinSigningKeyBytes (32). When empty in a wired-up
	// gateway, NewGateway generates a random key and logs a warning —
	// fine for dev, but a restart invalidates outstanding signed
	// URLs, so production should persist a configured value.
	PreviewSigningKey []byte
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
	previewSet := cfg.PreviewRoutes != nil
	hostsSet := cfg.PreviewHostLookup != nil
	rootSet := cfg.PreviewRootDomain != ""
	authSet := cfg.PreviewAuthenticator != nil
	if previewSet != hostsSet || previewSet != rootSet || previewSet != authSet {
		return nil, fmt.Errorf("gateway: PreviewRoutes, PreviewHostLookup, PreviewRootDomain, and PreviewAuthenticator must all be set together (got routes=%t hosts=%t root=%t auth=%t)", previewSet, hostsSet, rootSet, authSet)
	}
	if previewSet {
		if len(cfg.PreviewSigningKey) == 0 {
			generated, err := tokens.GenerateSigningKey()
			if err != nil {
				return nil, fmt.Errorf("gateway: generate preview signing key: %w", err)
			}
			cfg.PreviewSigningKey = generated
			warn := lg
			if warn == nil {
				warn = log.Default()
			}
			warn.Printf("gateway: PreviewSigningKey not configured — generated a random one. Signed URLs will not survive gateway restarts.")
		} else if len(cfg.PreviewSigningKey) < tokens.MinSigningKeyBytes {
			return nil, fmt.Errorf("gateway: PreviewSigningKey must be at least %d bytes (got %d)", tokens.MinSigningKeyBytes, len(cfg.PreviewSigningKey))
		}
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
	// /v1/worktrees/create and /v1/worktrees/list-branches must be
	// mounted BEFORE the `/v1/` catch-all so they reach the host (via
	// these gateway-orchestrated handlers) instead of the sync server.
	mx.HandleFunc("POST /v1/worktrees/create", g.handleCreateWorktree)
	mx.HandleFunc("POST /v1/worktrees/list-branches", g.handleListBranches)

	// GitHub Connect: status/disconnect/connect-flow/create-PR are
	// all pure proxies to the user's host. Mounted before the /v1/
	// catch-all for the same reason as the worktree routes above.
	mx.HandleFunc("GET /v1/github/status", g.handleGitHubStatus)
	mx.HandleFunc("DELETE /v1/github", g.handleGitHubDisconnect)
	mx.HandleFunc("POST /v1/github/connect/start", g.handleGitHubConnectStart)
	mx.HandleFunc("GET /v1/github/connect/status", g.handleGitHubConnectStatus)
	mx.HandleFunc("POST /v1/github/connect/cancel", g.handleGitHubConnectCancel)
	mx.HandleFunc("POST /v1/worktrees/{id}/pr", g.handleGitHubCreatePR)
	mx.HandleFunc("POST /v1/worktrees/{id}/pr/preview", g.handleGitHubPreviewPR)
	if g.cfg.Sync != nil {
		// POST /v1/migrate/worktrees/{id} is more specific and wins
		// over the /v1/ prefix registered here.
		mx.Handle("/v1/", g.cfg.Sync.Handler())
	}
	if g.cfg.Notify != nil {
		// /devices: user-scoped, inherits outer auth wrap.
		// /webhooks/notifications lives on the daemon's parent mux
		// (pre-auth) because the dispatcher does host-bearer
		// verification itself; see NotifyWebhookHandler.
		mx.HandleFunc("POST /devices", g.cfg.Notify.HandleRegister)
		mx.HandleFunc("DELETE /devices/{token}", g.cfg.Notify.HandleDeregister)
	}
	if g.cfg.PreviewRoutes != nil {
		// Owner-facing token management. Inherits the outer auth wrap;
		// handlers pull Principal from r.Context() and assert ownership
		// against the route's owner_user_id.
		mx.HandleFunc("GET /v1/preview/tokens", g.handleListPreviewTokens)
		mx.HandleFunc("POST /v1/preview/tokens/{token}/share", g.handleSharePreviewToken)
		mx.HandleFunc("POST /v1/preview/tokens/{token}/sign", g.handleSignPreviewToken)
		mx.HandleFunc("DELETE /v1/preview/tokens/{token}", g.handleDeletePreviewToken)
		// /webhooks/preview/* mounts pre-auth via PreviewWebhookHandler.
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

// NotifyWebhookHandler returns the dispatcher's host-side webhook
// handler when Config.Notify is set, otherwise nil. Daemons mount it
// PRE-auth on POST /webhooks/notifications — the dispatcher verifies
// the host bearer token itself, and the outer user-auth middleware
// would 401 the host call before it ever reached the dispatcher.
//
// Returning nil when Notify is unset mirrors AuthConfigHandler so
// callers can wire the route conditionally — `if h :=
// gw.NotifyWebhookHandler(); h != nil { mux.Handle("POST
// /webhooks/notifications", h) }`.
func (g *Gateway) NotifyWebhookHandler() http.Handler {
	if g.cfg.Notify == nil {
		return nil
	}
	return http.HandlerFunc(g.cfg.Notify.Handle)
}

// WrapPreviewSubdomain returns an http.Handler that dispatches by
// Host header: requests to preview-<token>.<PreviewRootDomain> hit
// the tokenized-URL proxy (which does its own per-token auth
// depending on visibility), and every other request falls through
// to fallback (typically the auth-wrapped main mux).
//
// When PreviewRoutes is unset the wrapper is a no-op: it returns
// fallback unchanged so daemons can call this unconditionally
// without branching on preview-config presence.
//
// This is the OUTERMOST layer at the daemon's parent mux because
// public-visibility tokens must accept anonymous requests; if we
// went through the outer JWT middleware first, those public URLs
// would 401 before reaching our visibility check.
func (g *Gateway) WrapPreviewSubdomain(fallback http.Handler) http.Handler {
	if g.cfg.PreviewRoutes == nil {
		return fallback
	}
	return g.previewSubdomainHandler(fallback)
}

// PreviewWebhookHandler returns the sprite-facing register/revoke
// router for /webhooks/preview/*, or nil when PreviewRoutes is unset.
// Daemons mount this PRE-auth so the per-host notifier_token bearer
// (which the handler verifies itself) reaches the resolver — the
// outer user-JWT middleware would 401 the host call outright.
//
// Routes mounted on the returned handler:
//
//	POST /webhooks/preview/register  →  upsert (host_id, wid, svc) → {token, url, expires_at}
//	POST /webhooks/preview/revoke    →  RevokeByService (idempotent)
func (g *Gateway) PreviewWebhookHandler() http.Handler {
	if g.cfg.PreviewRoutes == nil {
		return nil
	}
	mx := http.NewServeMux()
	mx.HandleFunc("POST /webhooks/preview/register", g.handlePreviewWebhookRegister)
	mx.HandleFunc("POST /webhooks/preview/revoke", g.handlePreviewWebhookRevoke)
	return mx
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
