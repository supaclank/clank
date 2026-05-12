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
	"fmt"
	"log"
	"net/http"

	"github.com/acksell/clank/pkg/provisioner"
	clanksync "github.com/acksell/clank/pkg/sync"
)

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
}

// Gateway is the public ingress.
type Gateway struct {
	cfg Config
	log *log.Logger

	// migrationKey signs two-phase migration tokens. Random-on-startup
	// so a daemon restart invalidates any pending materialize → commit
	// in flight; the laptop re-runs `clank pull --migrate`.
	migrationKey []byte
}

// NewGateway constructs a Gateway.
func NewGateway(cfg Config, lg *log.Logger) (*Gateway, error) {
	if cfg.Provisioner == nil {
		return nil, fmt.Errorf("gateway: Provisioner is required")
	}
	if lg == nil {
		lg = log.Default()
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("gateway: generate migration signing key: %w", err)
	}
	return &Gateway{cfg: cfg, log: lg, migrationKey: key}, nil
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
