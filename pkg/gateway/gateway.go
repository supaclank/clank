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

	"github.com/supaclank/clank/pkg/auth"
	"github.com/supaclank/clank/pkg/images"
	"github.com/supaclank/clank/pkg/notify"
	"github.com/supaclank/clank/pkg/preview/routestore"
	"github.com/supaclank/clank/pkg/preview/tokens"
	"github.com/supaclank/clank/pkg/provisioner"
	"github.com/supaclank/clank/pkg/provisioner/hoststore"
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

// Config wires the gateway's dependencies. Provisioner is required.
//
// Authentication is the responsibility of an outer middleware (see
// pkg/auth.Middleware) — by the time a request reaches the gateway,
// the verified Principal is already in r.Context().
type Config struct {
	// Provisioner resolves a userID into the user's HostRef. EnsureHost
	// is called per-request; the provisioner caches in-process.
	Provisioner provisioner.Provisioner

	// Images is the embedded image-upload presign server. When non-nil,
	// the gateway mounts POST /v1/images. When nil, the route isn't
	// mounted (404).
	Images *images.Server

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
// /ping and /gateway/health answer locally without waking a host; the
// /v1/* routes below are gateway-orchestrated (mostly pure proxies to
// the user's host); every other path proxies to the user's host
// verbatim. Authentication is handled by an outer middleware
// (pkg/auth.Middleware); handlers read the Principal from r.Context()
// via auth.MustPrincipal.
func (g *Gateway) Handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("GET /ping", g.handlePing)
	mx.HandleFunc("GET /gateway/health", g.handleGatewayHealth)
	// Brand-new project scaffolding: pure host proxies. The host owns
	// the template catalog (builtin config + the user's GitHub
	// template repos); clients pick an entry and send its clone_url.
	mx.HandleFunc("GET /v1/templates", g.handleListTemplates)
	mx.HandleFunc("POST /v1/projects/create", g.handleCreateProject)
	// Import an existing GitHub repo: clone owner/repo (with the host's
	// stored GitHub token) into a fresh worktree. See projects_import.go.
	mx.HandleFunc("POST /v1/projects/import", g.handleImportProject)
	// Worktree delete: pure host proxy — the host purges the worktree's
	// sessions and unlinks ~/work/{id} from its repo canonical.
	mx.HandleFunc("DELETE /v1/worktrees/{id}", g.handleDeleteWorktree)
	// GDPR/app-store account erasure: destroy the caller's compute (which
	// holds all repo + session state), devices, and preview routes.
	mx.HandleFunc("DELETE /v1/account", g.handleDeleteAccount)

	// GitHub Connect: status/disconnect/connect-flow/create-PR are
	// all pure proxies to the user's host.
	mx.HandleFunc("GET /v1/github/status", g.handleGitHubStatus)
	mx.HandleFunc("GET /v1/github/repos", g.handleGitHubListRepos)
	mx.HandleFunc("GET /v1/github/repos/{owner}/{repo}/branches", g.handleGitHubListBranches)
	mx.HandleFunc("GET /v1/github/repos/{owner}/{repo}/pulls", g.handleGitHubListPulls)
	mx.HandleFunc("DELETE /v1/github", g.handleGitHubDisconnect)
	mx.HandleFunc("POST /v1/github/connect/start", g.handleGitHubConnectStart)
	mx.HandleFunc("GET /v1/github/connect/status", g.handleGitHubConnectStatus)
	mx.HandleFunc("POST /v1/github/connect/cancel", g.handleGitHubConnectCancel)
	mx.HandleFunc("POST /v1/worktrees/{id}/pr", g.handleGitHubCreatePR)
	mx.HandleFunc("POST /v1/worktrees/{id}/pr/preview", g.handleGitHubPreviewPR)
	mx.HandleFunc("POST /v1/worktrees/{id}/pr/ready", g.handleGitHubMarkPRReady)
	mx.HandleFunc("POST /v1/github/pull-requests/inspect", g.handleGitHubPullRequestInspect)
	mx.HandleFunc("POST /v1/github/pull-requests/launch", g.handleGitHubPullRequestLaunch)
	mx.HandleFunc("POST /v1/github/repositories/inspect", g.handleGitHubRepositoryInspect)
	mx.HandleFunc("POST /v1/github/repositories/launch", g.handleGitHubRepositoryLaunch)

	// Repo-first surface: filesystem-derived listing, repo-scoped
	// worktree creation, the branch∪PR overview, and whole-repo delete.
	// Pure proxies with verbatim status forwarding — see
	// pkg/gateway/repos_proxy.go + internal/host/mux/repos.go.
	mx.HandleFunc("GET /v1/repos", g.handleReposList)
	mx.HandleFunc("POST /v1/repos/{slug}/worktrees", g.handleRepoWorktreeCreate)
	mx.HandleFunc("GET /v1/repos/{slug}/overview", g.handleRepoOverview)
	mx.HandleFunc("DELETE /v1/repos/{slug}", g.handleRepoDelete)

	// Worktree↔GitHub-remote sync. See pkg/gateway/remote_sync.go +
	// internal/host/mux/remote.go.
	mx.HandleFunc("GET /v1/worktrees/{id}/remote/status", g.handleRemoteStatus)
	mx.HandleFunc("POST /v1/worktrees/{id}/remote/push", g.handleRemotePush)
	mx.HandleFunc("POST /v1/worktrees/{id}/remote/pull", g.handleRemotePull)
	mx.HandleFunc("POST /v1/worktrees/{id}/remote/resolve", g.handleRemoteResolve)
	mx.HandleFunc("POST /v1/worktrees/{id}/remote/publish", g.handleRemotePublish)
	if g.cfg.Images != nil {
		// POST /v1/images: image-upload presign.
		mx.Handle("/v1/images", g.cfg.Images.Handler())
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
