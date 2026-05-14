package gateway

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/acksell/clank/pkg/auth"
)

// proxyToHost resolves the userID (from the verified Principal in
// context), asks the provisioner for the user's HostRef, and reverse-
// proxies through. HostRef.Transport injects per-request upstream auth.
// ReverseProxy upgrades HTTP/1.1 natively, so WebSocket (/events)
// flows through this same path. Bearer auth happens once at the TCP
// edge (pkg/auth.Middleware), so we don't re-verify here.
//
// Sync-path policy: when Sync is unconfigured (laptop mode), refuse to
// proxy code-sync routes (/sync, /sync/build, /sync/builds/*,
// /sync/apply-from-urls) to the local clank-host. The host registers
// those routes for sandbox use; on a laptop they'd let any client with
// socket access write code into ~/work/. Cloud gateways (Sync != nil)
// keep proxying so sprite-side handlers still work.
//
// /sync/sessions/* is allowed through on laptop gateways: those
// handlers operate on opencode storage + host.db, never on ~/work/.
// They're how the laptop's CLI drives session export/import during
// `clank push --migrate`.
func (g *Gateway) proxyToHost(w http.ResponseWriter, r *http.Request) {
	// Check the effective (post-strip) path: a request to
	// /hosts/<host>/sync/* would otherwise bypass the guard and reach
	// the host mux's unconditional /sync/* handlers.
	effectivePath := stripHostsPrefix(r.URL.Path)
	if g.cfg.Sync == nil && isBlockedCodeSyncRoute(effectivePath) {
		g.log.Printf("gateway: denied %s %s (code-sync routes blocked on laptop gateway)", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}

	userID := auth.MustPrincipal(r.Context()).UserID

	ref, err := g.cfg.Provisioner.EnsureHost(r.Context(), userID)
	if err != nil {
		g.log.Printf("gateway: ensure host for user %s: %v", userID, err)
		http.Error(w, "host unavailable", http.StatusBadGateway)
		return
	}

	target, err := url.Parse(ref.URL)
	if err != nil {
		g.log.Printf("gateway: invalid host URL %q for user %s: %v", ref.URL, userID, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

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
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			g.log.Printf("gateway: proxy %s %s: %v", req.Method, req.URL.Path, err)
			http.Error(rw, "upstream error", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// isBlockedCodeSyncRoute reports whether p is a /sync/* route that
// writes to ~/work/<repo>/ on the host — the family of routes we
// firewall on laptop gateways. /sync/sessions/* is deliberately
// allowed because those handlers don't touch ~/work/; they call into
// the host Service to drive opencode export/import + host.db upserts.
func isBlockedCodeSyncRoute(p string) bool {
	if p != "/sync" && !strings.HasPrefix(p, "/sync/") {
		return false
	}
	if p == "/sync/sessions" || strings.HasPrefix(p, "/sync/sessions/") {
		return false
	}
	return true
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
