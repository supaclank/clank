package webpreview

import (
	"net/http"
	"net/url"
	"strings"
)

// Browser-facing origin headers, restated in the upstream dev server's
// own terms.
//
// Dev servers gate their internals on the origin they were started on:
// Next blocks cross-origin /_next/* requests unless the origin is in
// allowedDevOrigins, Vite demands a token on origin-bearing requests,
// and framework CSRF checks compare Origin against Host. Behind this
// proxy that origin is stale — the browser talks to the preview host
// while the dev server only ever knows its loopback address — so
// requests the browser itself marked same-origin arrive looking foreign
// and get rejected. CORS-mode subresources are the usual casualty:
// Next tags two of its bootstrap chunks `crossorigin`, which is what
// makes the browser attach Origin at all.
//
// This is the peer of the Host rewrite in newUpstreamProxy — both
// restate the browser-facing origin as the upstream's. Only a header
// that already names the inbound Host is translated, so a genuinely
// cross-site request still reaches the dev server foreign and is still
// refused there. Origin is a forbidden header name, so page scripts
// cannot forge the match.
const (
	headerOrigin  = "Origin"
	headerReferer = "Referer"
	// originOpaque is what browsers send for sandboxed or
	// privacy-sensitive origins. It names no host, so it can never be
	// proven same-origin.
	originOpaque = "null"
)

// rewriteBrowserOrigin translates Origin and Referer from inboundHost
// to target's origin. Headers naming any other host are left untouched.
func rewriteBrowserOrigin(out *http.Request, inboundHost string, target *url.URL) {
	if inboundHost == "" || target == nil {
		return
	}
	if origin := out.Header.Get(headerOrigin); origin != originOpaque && namesHost(origin, inboundHost) {
		out.Header.Set(headerOrigin, target.Scheme+"://"+target.Host)
	}
	if referer := out.Header.Get(headerReferer); namesHost(referer, inboundHost) {
		// Keep path and query: Referer is a full URL, and only its
		// origin is stale.
		u, err := url.Parse(referer)
		if err == nil {
			u.Scheme, u.Host = target.Scheme, target.Host
			out.Header.Set(headerReferer, u.String())
		}
	}
}

// namesHost reports whether raw is an absolute URL whose host[:port] is
// host. A relative, empty, or malformed value names no host and must
// never match — matching is what grants the rewrite.
func namesHost(raw, host string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}
