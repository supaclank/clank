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
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/images"
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

// IdPDeleter optionally deletes or disables a user in the operator's
// external SSO/identity provider when their clank account is deleted.
// clank itself only verifies tokens (pkg/auth.Authenticator) and has no
// IdP write access, so this is an extension point: when Config.IdPDeleter
// is nil the account-deletion endpoint skips the IdP step. Operators wire
// a concrete implementation (Supabase admin API, Auth0 Management API, …);
// they choose delete-vs-disable semantics inside DeleteUser.
type IdPDeleter interface {
	DeleteUser(ctx context.Context, userID string) error
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
// Sync is optional (when nil, the pull route returns 503 and the /v1/
// prefix isn't mounted).
//
// Authentication is the responsibility of an outer middleware (see
// pkg/auth.Middleware) — by the time a request reaches the gateway,
// the verified Principal is already in r.Context().
type Config struct {
	// Provisioner resolves a userID into the user's HostRef. EnsureHost
	// is called per-request; the provisioner caches in-process.
	Provisioner provisioner.Provisioner

	// Sync is the embedded sync server. When non-nil, the gateway mounts
	// the sync API routes under /v1/ and the pull route calls sync
	// methods directly rather than via HTTP. When nil, the pull route
	// returns 503.
	Sync *clanksync.Server

	// Images is the embedded image-upload presign server. When non-nil,
	// the gateway mounts POST /v1/images (more specific than Sync's /v1/
	// catch-all, so it wins). Independent of Sync — its own bucket. When
	// nil, /v1/images falls through to the sync server (404).
	Images *images.Server

	// Templates is the catalog of built-in project templates a user can
	// scaffold a brand-new project from (POST /v1/projects/create). Each
	// entry maps a stable id to a clone URL; the operator injects these
	// at deploy time so neither the id nor the URL is hardcoded in OSS.
	// Empty means project creation is unavailable (404 on unknown id).
	Templates []Template

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
	// All four must be set together or none — leaving any nil
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

	// IdPDeleter, when non-nil, is invoked as the final step of
	// DELETE /v1/account to delete/disable the user in the operator's
	// external SSO. Optional — nil skips the IdP step (clank-data-only
	// deletion). See the IdPDeleter interface.
	IdPDeleter IdPDeleter
}

// Gateway is the public ingress.
type Gateway struct {
	cfg Config
	log *log.Logger
}

// NewGateway constructs a Gateway.
func NewGateway(cfg Config, lg *log.Logger) (*Gateway, error) {
	if cfg.Provisioner == nil {
		return nil, fmt.Errorf("gateway: Provisioner is required")
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
	return &Gateway{cfg: cfg, log: lg}, nil
}

// Handler returns the public-listener http.Handler.
//
// /ping and /gateway/health answer locally without waking a host;
// /v1/worktrees/{id}/pull runs the gateway-orchestrated pull flow when
// Sync is configured; /v1/ (other paths) forwards to the embedded sync
// server when Sync is configured; every other path proxies to the
// user's host. Authentication is handled by an outer middleware
// (pkg/auth.Middleware); handlers read the Principal from r.Context()
// via auth.MustPrincipal.
func (g *Gateway) Handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("GET /ping", g.handlePing)
	mx.HandleFunc("GET /gateway/health", g.handleGatewayHealth)
	mx.HandleFunc("POST /v1/worktrees/{id}/pull", g.handlePullWorktree)
	// /v1/worktrees/create and /v1/worktrees/list-branches must be
	// mounted BEFORE the `/v1/` catch-all so they reach the host (via
	// these gateway-orchestrated handlers) instead of the sync server.
	mx.HandleFunc("POST /v1/worktrees/create", g.handleCreateWorktree)
	mx.HandleFunc("POST /v1/worktrees/list-branches", g.handleListBranches)
	// Brand-new project scaffolding. GET lists the template catalog;
	// POST resolves a template id to its clone URL and asks the host to
	// scaffold it. Mounted before the /v1/ catch-all for the same reason
	// as the worktree routes above.
	mx.HandleFunc("GET /v1/templates", g.handleListTemplates)
	mx.HandleFunc("POST /v1/projects/create", g.handleCreateProject)
	// Autosync (S3→sprite): sync-all (mobile homescreen) + per-worktree
	// (manual sync button / conflict resolution). Mounted before the /v1/
	// catch-all so they reach these gateway-orchestrated handlers.
	mx.HandleFunc("POST /v1/worktrees/sync", g.handleSyncAllWorktrees)
	mx.HandleFunc("POST /v1/worktrees/{id}/sync", g.handleSyncWorktree)
	// Full-cleanup delete: strip the sprite's materialized copy + sessions,
	// then delete the sync row + checkpoints. Mounted before the `/v1/`
	// catch-all so DELETE reaches this gateway handler, not the sync server.
	mx.HandleFunc("DELETE /v1/worktrees/{id}", g.handleDeleteWorktree)
	// GDPR/app-store account erasure: destroy the caller's compute, purge
	// their sync data + object-store blobs, devices, and preview routes.
	// Mounted before the `/v1/` catch-all for the same reason.
	mx.HandleFunc("DELETE /v1/account", g.handleDeleteAccount)

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
	if g.cfg.Images != nil {
		// POST /v1/images: image-upload presign. More specific than the
		// sync /v1/ catch-all below, so it wins regardless of order.
		mx.Handle("/v1/images", g.cfg.Images.Handler())
	}
	if g.cfg.Sync != nil {
		// The specific /v1/worktrees/... routes above are more specific
		// and win over this /v1/ prefix registered here.
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
	// All /sessions* ops proxy to the user's host (the laptop's local
	// clank-host, or the sandbox for a cloud gateway). The gateway no
	// longer routes per-worktree local-vs-remote — that ownership-based
	// routing is gone.
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
