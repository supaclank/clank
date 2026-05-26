package preview

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// newReverseProxy builds the per-server reverse proxy. Returns the
// underlying httputil.ReverseProxy so Manager can compose extra
// middleware (keepalive bump, last-touch bump) at the ServeHTTP layer.
//
// Three design choices baked in:
//
//   - Strip Authorization, Cookie, X-Forwarded-Host before forwarding.
//     The dev server (Metro, Vite, …) is running user code and must
//     never see the sprite bearer or any clank session cookies. Same
//     defense pkg/gateway/remote_proxy.go applies one hop earlier.
//
//   - FlushInterval: -1 so HMR WebSockets and Metro's SSE event log
//     stream don't buffer. ReverseProxy's native HTTP/1.1 Upgrade path
//     handles the WS upgrade unchanged.
//
//   - ModifyResponse rewrites any URL string in Metro's manifest that
//     points at our public origin (host:port) to include the path
//     prefix the proxy is mounted under. Without this, manifest URLs
//     like "http://gateway/node_modules/X/entry.bundle" arrive at the
//     gateway with no prefix, route nowhere, and the bundle 404s.
//     EXPO_PACKAGER_PROXY_URL only fixes the host:port portion — Metro
//     still constructs paths from scratch.
func newReverseProxy(port int, publicURL string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}
	pub, err := url.Parse(publicURL)
	if err != nil {
		return nil, fmt.Errorf("parse public URL: %w", err)
	}
	// publicOrigin is what Metro will bake into manifest URLs (via
	// EXPO_PACKAGER_PROXY_URL). publicPath is what we need to prepend
	// to those URLs so they land back at this proxy through clank-host.
	publicOrigin := pub.Scheme + "://" + pub.Host
	publicPath := strings.TrimRight(pub.Path, "/")
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("X-Forwarded-Host")
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			return rewriteJSONURLs(resp, publicOrigin, publicPath)
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			// The dev server crashed mid-request or was reaped between
			// the path lookup and the dial. Surface as 502 so HMR
			// clients reconnect (Metro retries the WS on its own).
			http.Error(w, "preview upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}, nil
}

// rewriteJSONURLs string-substitutes any occurrence of publicOrigin/<path>
// in JSON response bodies with publicOrigin/<publicPath>/<path>. Skips
// non-JSON content (assets like .bundle, images, source maps stream
// through unchanged) so HMR + bundle bytes aren't disturbed.
//
// Idempotent: if a URL already starts with publicOrigin+publicPath, it's
// left alone — re-prefixing would produce ".../<prefix>/<prefix>/..."
// on repeated requests.
func rewriteJSONURLs(resp *http.Response, publicOrigin, publicPath string) error {
	if publicPath == "" {
		return nil // nothing to prepend
	}
	ctype := resp.Header.Get("Content-Type")
	if !strings.Contains(ctype, "json") {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := resp.Body.Close(); err != nil {
		return err
	}

	// `publicOrigin + "/"` is the marker we look for. Replace with
	// `publicOrigin + publicPath + "/"`. Pre-prefixed URLs are
	// idempotent because they contain `publicOrigin + publicPath + "/"`
	// — the From string is a substring of the To string, so a second
	// pass would double the prefix. Guard with a one-pass replacement
	// that skips matches already followed by publicPath.
	src := []byte(publicOrigin + "/")
	alreadyPrefixed := []byte(publicOrigin + publicPath + "/")

	var out bytes.Buffer
	out.Grow(len(body) + 64) // small extra for prefix insertions
	i := 0
	for i < len(body) {
		idx := bytes.Index(body[i:], src)
		if idx < 0 {
			out.Write(body[i:])
			break
		}
		matchStart := i + idx
		out.Write(body[i:matchStart])
		// Skip if this match is actually the start of an
		// already-prefixed URL (publicOrigin + publicPath + "/").
		if bytes.HasPrefix(body[matchStart:], alreadyPrefixed) {
			out.Write(alreadyPrefixed)
			i = matchStart + len(alreadyPrefixed)
			continue
		}
		out.WriteString(publicOrigin)
		out.WriteString(publicPath)
		out.WriteByte('/')
		i = matchStart + len(src)
	}

	resp.Body = io.NopCloser(&out)
	resp.ContentLength = int64(out.Len())
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", out.Len()))
	return nil
}
