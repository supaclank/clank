// Package gateway is the daemon's single ingress: it authenticates,
// resolves the user to a persistent host via the provisioner, and
// reverse-proxies everything else through.
//
// Routing: /ping and /gateway/health are served locally; every other
// path proxies to the user's host with the provisioner-supplied
// transport injecting per-request auth.
package gateway

import (
	"fmt"
	"log"
	"net/http"

	"github.com/acksell/clank/pkg/provisioner"
)

// Config wires the gateway's dependencies. All fields except
// Provisioner have sensible defaults.
type Config struct {
	// Provisioner resolves a userID into the user's HostRef. EnsureHost
	// is called per-request; the provisioner caches in-process.
	Provisioner provisioner.Provisioner

	// Auth verifies the bearer on incoming requests. Defaults to
	// PermissiveAuth (any bearer accepted).
	Auth Authenticator

	// ResolveUserID maps a verified request to a userID. Defaults to
	// returning "local".
	ResolveUserID func(*http.Request) string
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
	if cfg.Auth == nil {
		cfg.Auth = PermissiveAuth{}
	}
	if cfg.ResolveUserID == nil {
		cfg.ResolveUserID = func(*http.Request) string { return "local" }
	}
	if lg == nil {
		lg = log.Default()
	}
	return &Gateway{cfg: cfg, log: lg}, nil
}

// Handler returns the public-listener http.Handler.
//
// /ping and /gateway/health answer locally without waking a host;
// every other path proxies to the user's host.
func (g *Gateway) Handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("GET /ping", g.handlePing)
	mx.HandleFunc("GET /gateway/health", g.handleGatewayHealth)
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
