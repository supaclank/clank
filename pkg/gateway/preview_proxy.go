// preview_proxy.go — tokenized-URL subdomain proxy.
//
// Request path: client → "preview-<token>.<root>" → this handler →
// gateway's Provisioner.OpenInternalConn → Metro (or other dev
// server) inside the sprite.
//
// Auth model:
//   - Owner-only tokens: require JWT (via the same Authenticator the
//     main mux uses) and assert principal.sub == route.owner_user_id.
//     Cross-tenant attempts surface as 404 (not 403) — never leak
//     "this token exists but isn't yours."
//   - Public tokens: no auth check. The URL itself is the
//     capability; the gateway treats anyone with the link as a
//     legitimate viewer.
//
// Lifecycle: per-(host_id, port) tunnels are pooled lazily and never
// explicitly evicted from this map. Stdlib http.Transport's
// IdleConnTimeout closes the inner WSS connections after they go
// unused; the empty Tunnel wrapper remains in the pool but consumes
// negligible memory. A sprite suspend doesn't need active eviction —
// the next request opens a fresh WSS to api.sprites.dev which wakes
// the sprite (Sprites edge auto-wake).
package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/gateway/previewtunnel"
	"github.com/acksell/clank/pkg/preview/routestore"
	"github.com/acksell/clank/pkg/preview/tokens"
)

// previewSubdomainHandler returns the http.Handler that dispatches
// preview hosts (and otherwise delegates to fallback).
func (g *Gateway) previewSubdomainHandler(fallback http.Handler) http.Handler {
	state := &previewState{
		gw:         g,
		pool:       &sync.Map{},
		root:       g.cfg.PreviewRootDomain,
		auth:       g.cfg.PreviewAuthenticator,
		store:      g.cfg.PreviewRoutes,
		signingKey: g.cfg.PreviewSigningKey,
		now:        time.Now,
		log:        g.log,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := tokens.ParseHost(r.Host, state.root)
		if !ok {
			fallback.ServeHTTP(w, r)
			return
		}
		state.serveToken(w, r, token)
	})
}

// previewState bundles the per-Gateway preview wiring so the
// HandlerFunc closure stays tiny. One instance lives in the closure
// returned by previewSubdomainHandler; the pool is shared across all
// requests.
type previewState struct {
	gw         *Gateway
	pool       *sync.Map // tunnelKey → *previewtunnel.Tunnel
	root       string
	auth       auth.Authenticator
	store      routestore.Store
	signingKey []byte
	now        func() time.Time
	log        logger
}

// logger is the subset of *log.Logger that previewState uses. Keeps
// the closure parameterizable in tests; *log.Logger satisfies it.
type logger interface {
	Printf(format string, args ...any)
}

// tunnelKey is the pool key. Two preview routes with the same
// (host_id, port) share a Tunnel — the bytes are interchangeable.
type tunnelKey struct {
	HostID string
	Port   int
}

func (s *previewState) serveToken(w http.ResponseWriter, r *http.Request, token string) {
	route, err := s.store.GetByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, routestore.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Printf("preview proxy: store get-by-token: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Visibility gate.
	switch route.Visibility {
	case tokens.VisibilityPublic:
		// Anonymous access allowed.
	case tokens.VisibilityOwnerOnly:
		if !s.authorizeOwnerOnly(w, r, route, token) {
			return
		}
	default:
		s.log.Printf("preview proxy: route %s has unknown visibility %q", route.Token, route.Visibility)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	tun, err := s.tunnelFor(route)
	if err != nil {
		s.log.Printf("preview proxy: tunnel build: %v", err)
		http.Error(w, "preview backend unavailable", http.StatusBadGateway)
		return
	}

	s.serveProxy(w, r, tun, route)
}

// authorizeOwnerOnly checks the request for either a valid
// signed-URL bearer (query first, cookie second) or a JWT whose
// principal matches the route's owner. Writes the failure response
// (401 / 404) and returns false on rejection; returns true to allow
// the proxy to continue.
//
// On a query-supplied bearer (the dev-launcher first-touch), the
// proxy also Set-Cookies sig + exp scoped to the preview host so
// subsequent runtime fetches don't need to re-carry the query
// params. This is the "bridge" between the URL bearer (which mobile
// can manage) and the runtime fetches (which mobile can't add
// headers to).
func (s *previewState) authorizeOwnerOnly(w http.ResponseWriter, r *http.Request, route routestore.Route, token string) bool {
	// 1. Try the signed bearer (query or cookie). Most requests on a
	// healthy preview session land here, not the JWT path.
	if len(s.signingKey) > 0 {
		switch err := tokens.VerifyFromRequest(s.signingKey, token, r, s.now()); {
		case err == nil:
			// On the query-supplied first-touch, write Set-Cookie so
			// subsequent runtime fetches carry the bearer without the
			// caller having to thread it through every URL.
			if r.URL.Query().Get(tokens.SigParam) != "" {
				if sig, exp, ok := signedQueryFromRequest(r); ok {
					// Inspect the incoming request to decide whether
					// Secure should be set on the outgoing cookies.
					// Plain-HTTP local dev wouldn't store Secure
					// cookies; production-TLS does. Either way the
					// cookies are HttpOnly + SameSite=Strict.
					tokens.SetSignedCookies(w, sig, exp, tokens.RequestIsHTTPS(r))
				}
			}
			return true
		case errors.Is(err, tokens.ErrSignatureExpired):
			// Don't fall through to JWT: an expired signature is a
			// definite "no" rather than "try another credential."
			http.Error(w, "signed preview URL expired", http.StatusUnauthorized)
			return false
		case errors.Is(err, tokens.ErrInvalidSignature):
			// No signature OR a malformed one. Either way we let the
			// JWT path try; the dev-launcher can't supply a JWT but a
			// well-behaved API client might.
		default:
			s.log.Printf("preview proxy: signature verify error: %v", err)
		}
	}

	// 2. JWT path (API clients that carry Authorization).
	principal, err := s.auth.Verify(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="clank"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if principal.UserID != route.OwnerUserID {
		// Deliberately 404 (not 403) — don't leak the existence
		// of someone else's token through the error code.
		http.NotFound(w, r)
		return false
	}
	return true
}

// signedQueryFromRequest extracts sig+exp from the query when both
// are present and exp is well-formed. Used after a successful
// VerifyFromRequest to feed SetSignedCookies — VerifyFromRequest
// itself doesn't expose the parsed pair.
func signedQueryFromRequest(r *http.Request) (sig string, exp time.Time, ok bool) {
	q := r.URL.Query()
	sig = q.Get(tokens.SigParam)
	expStr := q.Get(tokens.ExpParam)
	if sig == "" || expStr == "" {
		return "", time.Time{}, false
	}
	t, err := parseUnixSeconds(expStr)
	if err != nil {
		return "", time.Time{}, false
	}
	return sig, t, true
}

// parseUnixSeconds duplicates the tokens.parseUnix helper (which is
// unexported there). Kept tiny so the duplication is cheaper than
// exporting another internal-ish helper.
func parseUnixSeconds(s string) (time.Time, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(n, 0), nil
}

// tunnelFor returns the pooled Tunnel for (host_id, port), creating
// one on first reference.
func (s *previewState) tunnelFor(route routestore.Route) (*previewtunnel.Tunnel, error) {
	key := tunnelKey{HostID: route.HostID, Port: route.InternalPort}
	if v, ok := s.pool.Load(key); ok {
		return v.(*previewtunnel.Tunnel), nil
	}
	tun, err := previewtunnel.New(s.gw.cfg.Provisioner, route.HostID, route.InternalPort, previewtunnel.Config{})
	if err != nil {
		return nil, fmt.Errorf("preview-tunnel: %w", err)
	}
	// LoadOrStore so a concurrent first-request race doesn't leak the
	// loser's Tunnel.
	actual, loaded := s.pool.LoadOrStore(key, tun)
	if loaded {
		tun.Close()
		return actual.(*previewtunnel.Tunnel), nil
	}
	return tun, nil
}

// serveProxy builds and runs the per-request reverse proxy. Each
// request gets its own *httputil.ReverseProxy because the Director
// closes over `route` for the upstream URL; the Tunnel (which holds
// the connection pool) is shared.
func (s *previewState) serveProxy(w http.ResponseWriter, r *http.Request, tun *previewtunnel.Tunnel, _ routestore.Route) {
	// upstream's Scheme/Host are placeholders — the Tunnel's
	// DialContext ignores them. We still need a valid URL for
	// ReverseProxy.Director to compose against.
	upstream := &url.URL{Scheme: "http", Host: "preview-upstream"}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			// Preserve the original Host: Metro reads it back into the
			// manifest URLs it emits, so the client sees its own
			// preview-<token>.<root> hostname round-trip. The whole
			// rewriter-deletion thesis hangs on this line.
			pr.Out.Host = pr.In.Host

			// Strip credentials before forwarding. Metro is user code;
			// it must never see the Supabase JWT, any clank session
			// cookie, or client-supplied X-Forwarded-* claims. Same
			// threat model as V1's sprite-side header strip, just moved
			// one hop earlier.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Proto")

			// Strip signed-URL bearer params so Metro never sees the
			// clank_sig/clank_exp credentials. SetURL above preserves
			// the inbound query string verbatim.
			q := pr.Out.URL.Query()
			q.Del(tokens.SigParam)
			q.Del(tokens.ExpParam)
			pr.Out.URL.RawQuery = q.Encode()
		},
		Transport: tun,
		// HMR + SSE both need byte-by-byte forwarding. -1 disables
		// the proxy's buffering. The Phase 0 spike confirmed WS
		// upgrade rides on this Transport unchanged.
		FlushInterval: -1,
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			s.log.Printf("preview proxy: upstream error on %s %s: %v", req.Method, req.URL.Path, err)
			http.Error(rw, "preview_upstream_error", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
