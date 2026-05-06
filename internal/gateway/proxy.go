package gateway

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// proxyToHost authenticates, resolves the userID, asks the provisioner
// for the user's HostRef, and reverse-proxies through. HostRef.Transport
// injects per-request upstream auth. ReverseProxy upgrades HTTP/1.1
// natively, so WebSocket (/events) flows through this same path.
func (g *Gateway) proxyToHost(w http.ResponseWriter, r *http.Request) {
	if _, err := g.cfg.Auth.Verify(r); err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="clank"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	userID := g.cfg.ResolveUserID(r)
	if userID == "" {
		http.Error(w, "no user identity", http.StatusUnauthorized)
		return
	}

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
