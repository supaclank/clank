// Package previewtunnel exposes the preview-app's HTTP transport: a
// thin wrapper around an stdlib *http.Transport whose DialContext
// opens a fresh net.Conn to a sprite's internal port via the
// configured Provisioner.
//
// The gateway's tokenized preview-URL proxy uses this. For one
// Metro/dev-server target (host_id, port), build one *Tunnel and
// hand its RoundTripper to httputil.ReverseProxy.
//
// Idle keep-alive reuse is disabled (DisableKeepAlives). The inner
// net.Conn is a WSS tunnel to api.sprites.dev; the Sprites edge
// idle-drops quiet tunnels and drops connections on sprite pause (per
// the Sprites docs, "open TCP connections drop on the pause"), leaving
// a pooled conn half-open — the next request hangs with no response.
// A fresh dial avoids the stale conn and cleanly wakes a paused sprite.
//
// NOTE: this is a network-transport concern only. The HMR WebSocket is
// a hijacked, long-lived connection that is never returned to the
// keep-alive pool, so DisableKeepAlives does not affect it.
//
// Per-host configuration knobs (timeouts) live on Config so the
// gateway doesn't have to litter call sites with magic numbers.
package previewtunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/acksell/clank/pkg/provisioner"
)

// Defaults applied when Config fields are zero.
const (
	defaultMaxIdleConns    = 8
	defaultIdleConnTimeout = 30 * time.Second
	// defaultDialTimeout caps a single fresh tunnel dial (incl. the
	// Sprites edge waking a paused sprite: ~1-2s cold per the docs, plus
	// the WSS handshake). Lowered from 10s so a genuinely-unreachable
	// sprite surfaces as a fast 502 (the client can then report
	// "offline" quickly) instead of a long hang. Cold-wake headroom kept.
	defaultDialTimeout = 5 * time.Second
)

// ErrUninitialized is returned by RoundTrip when a Tunnel is used
// after Close. Callers that need long-lived RoundTrippers should
// avoid Close and let the gateway drop the reference instead.
var ErrUninitialized = errors.New("previewtunnel: tunnel is closed")

// Config wires the per-Tunnel knobs. Zero values fall back to
// sensible defaults sized for one mobile client talking to one
// dev server.
type Config struct {
	// MaxIdleConns and IdleConnTimeout are retained for API stability
	// but are INERT: the transport runs with DisableKeepAlives, so no
	// connection is ever returned to the idle pool. See the package doc
	// for why idle reuse is disabled.
	MaxIdleConns int

	// IdleConnTimeout is inert (see MaxIdleConns / DisableKeepAlives).
	IdleConnTimeout time.Duration

	// DialTimeout caps each OpenInternalConn call. Hit, and the
	// returned error matches stdlib net dial-timeout semantics so
	// httputil.ReverseProxy's ErrorHandler can do its 502 dance.
	DialTimeout time.Duration
}

// Tunnel is an http.RoundTripper that dials every request through
// Provisioner.OpenInternalConn(hostID, port). Safe for concurrent
// use by multiple goroutines (stdlib *http.Transport semantics).
type Tunnel struct {
	prov   provisioner.Provisioner
	hostID string
	port   int
	rt     atomic.Pointer[http.Transport]
}

// New constructs a Tunnel ready to receive requests. Returns an
// error iff any required field is missing — fails fast so a bad
// wiring at gateway-startup time doesn't quietly degrade into 502s
// at request time.
func New(prov provisioner.Provisioner, hostID string, port int, cfg Config) (*Tunnel, error) {
	if prov == nil {
		return nil, fmt.Errorf("previewtunnel: provisioner is required")
	}
	if hostID == "" {
		return nil, fmt.Errorf("previewtunnel: hostID is required")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("previewtunnel: port %d out of range", port)
	}
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = defaultMaxIdleConns
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = defaultIdleConnTimeout
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}

	t := &Tunnel{prov: prov, hostID: hostID, port: port}
	t.rt.Store(&http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// Override the inbound addr (which the stdlib derives from
			// the request URL's host) — we always dial the configured
			// (hostID, port) regardless of what the proxy URL looked like.
			// The DialTimeout is enforced by wrapping ctx; the
			// Provisioner respects ctx for cancellation.
			dialCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
			defer cancel()
			return t.prov.OpenInternalConn(dialCtx, t.hostID, t.port)
		},
		MaxIdleConns:      cfg.MaxIdleConns,
		IdleConnTimeout:   cfg.IdleConnTimeout,
		DisableKeepAlives: true, // prevent stale half-open tunnels; see package doc
		// TLSClientConfig stays nil: Metro inside the sprite speaks
		// plain HTTP, and the public-edge TLS is terminated at the
		// gateway one hop earlier.
	})
	return t, nil
}

// RoundTrip implements http.RoundTripper by delegating to the
// stdlib transport whose DialContext opens tunnels through the
// Provisioner.
func (t *Tunnel) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := t.rt.Load()
	if rt == nil {
		return nil, ErrUninitialized
	}
	return rt.RoundTrip(req)
}

// Close releases the underlying connection pool. Safe to call
// multiple times; further RoundTrip calls return ErrUninitialized.
func (t *Tunnel) Close() {
	rt := t.rt.Swap(nil)
	if rt != nil {
		rt.CloseIdleConnections()
	}
}

// CloseIdleConnections lets gateway code drop idle tunnels without
// taking the Tunnel out of service (e.g. after a host suspend).
// Live in-flight requests are NOT disrupted.
func (t *Tunnel) CloseIdleConnections() {
	if rt := t.rt.Load(); rt != nil {
		rt.CloseIdleConnections()
	}
}
