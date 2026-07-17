package gateway

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/auth"
)

const (
	// slowEnsureHostThreshold flags EnsureHost calls that left the
	// in-process cache fast path (full resolve/reconcile/ready walk).
	slowEnsureHostThreshold = 250 * time.Millisecond
	// slowUpstreamThreshold flags proxied requests whose upstream took
	// long to produce response headers — the signature of a Flycast
	// dial absorbing a machine cold boot. Measured to headers, not
	// body end, so long-lived streams (SSE) don't false-positive.
	slowUpstreamThreshold = 1 * time.Second
)

// proxyToHost resolves the userID (from the verified Principal in
// context), asks the provisioner for the user's HostRef, and reverse-
// proxies through. HostRef.Transport injects per-request upstream auth.
// ReverseProxy upgrades HTTP/1.1 natively, so WebSocket (/events)
// flows through this same path. Bearer auth happens once at the TCP
// edge (pkg/auth.Middleware), so we don't re-verify here.
func (g *Gateway) proxyToHost(w http.ResponseWriter, r *http.Request) {
	userID := auth.MustPrincipal(r.Context()).UserID

	ensureStart := time.Now()
	ref, err := g.cfg.Provisioner.EnsureHost(r.Context(), userID)
	if err != nil {
		g.log.Printf("gateway: ensure host for user %s after %s: %v",
			userID, time.Since(ensureStart).Round(time.Millisecond), err)
		http.Error(w, "host unavailable", http.StatusBadGateway)
		return
	}
	if d := time.Since(ensureStart); d > slowEnsureHostThreshold {
		g.log.Printf("gateway: ensure host user=%s took %s (left the cache fast path)",
			userID, d.Round(time.Millisecond))
	}

	target, err := url.Parse(ref.URL)
	if err != nil {
		g.log.Printf("gateway: invalid host URL %q for user %s: %v", ref.URL, userID, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	upstreamStart := time.Now()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Strip /hosts/{name}/ from the *incoming* path before
			// SetURL joins target.Path. If we strip after the join, a
			// target with a non-empty path (http://upstream/v1) yields
			// /v1/hosts/{name}/… which doesn't match the prefix and
			// silently leaks /hosts/{name} to the upstream.
			strippedPath := stripHostsPrefix(pr.In.URL.Path)
			strippedRaw := stripHostsPrefix(pr.In.URL.RawPath)
			pr.SetURL(target)
			// Forward the upstream Host: Sprites' edge routes by Host
			// header, and the client's Host (a tunnel hostname or
			// localhost) makes the edge serve its own 404 page.
			pr.Out.Host = target.Host
			pr.Out.URL.Path = singleJoiningSlash(target.Path, strippedPath)
			pr.Out.URL.RawPath = singleJoiningSlash(target.RawPath, strippedRaw)
		},
		Transport: ref.Transport,
		// Slow time-to-headers on a proxied request is the wake cost a
		// client actually experienced (Flycast dial + machine boot +
		// host handling) — the number that attributes cold-boot latency.
		ModifyResponse: func(resp *http.Response) error {
			if d := time.Since(upstreamStart); d > slowUpstreamThreshold {
				g.log.Printf("gateway: slow upstream %s %s -> %d headers after %s (user=%s)",
					r.Method, r.URL.Path, resp.StatusCode, d.Round(time.Millisecond), userID)
			}
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			g.log.Printf("gateway: proxy %s %s after %s: %v",
				req.Method, req.URL.Path, time.Since(upstreamStart).Round(time.Millisecond), err)
			http.Error(rw, "upstream error", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// stripHostsPrefix turns "/hosts/{name}/foo/bar" into "/foo/bar". A
// path not under /hosts/ (or an empty input) is returned unchanged so
// the RawPath case ("" when no encoded segments) needs no special-case.
func stripHostsPrefix(p string) string {
	if p == "" || !strings.HasPrefix(p, "/hosts/") {
		return p
	}
	rest := p[len("/hosts/"):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return "/"
}

// singleJoiningSlash mirrors net/http/httputil.singleJoiningSlash.
// We can't call SetURL with a stripped pr.In (it reads pr.In.URL.Path
// directly), so we rebuild the joined path here from target.Path +
// the stripped suffix.
func singleJoiningSlash(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
