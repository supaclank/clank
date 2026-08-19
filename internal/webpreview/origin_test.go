package webpreview

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRewriteBrowserOriginTranslatesOnlyTheInboundHost(t *testing.T) {
	t.Parallel()
	target, err := url.Parse("http://127.0.0.1:53685")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	const inbound = "preview-abc.supaclank.dev"

	tests := []struct {
		name        string
		inboundHost string
		origin      string
		referer     string
		wantOrigin  string
		wantReferer string
	}{
		{
			name:        "same origin is restated as upstream",
			inboundHost: inbound,
			origin:      "https://preview-abc.supaclank.dev",
			wantOrigin:  "http://127.0.0.1:53685",
		},
		{
			name:        "host match is case-insensitive",
			inboundHost: inbound,
			origin:      "https://PREVIEW-ABC.supaclank.dev",
			wantOrigin:  "http://127.0.0.1:53685",
		},
		{
			name:        "foreign origin is forwarded untouched so upstream still refuses it",
			inboundHost: inbound,
			origin:      "https://evil.example",
			wantOrigin:  "https://evil.example",
		},
		{
			name:        "a suffix of the inbound host is not the inbound host",
			inboundHost: inbound,
			origin:      "https://evil-preview-abc.supaclank.dev",
			wantOrigin:  "https://evil-preview-abc.supaclank.dev",
		},
		{
			name:        "a subdomain of the inbound host is not the inbound host",
			inboundHost: inbound,
			origin:      "https://preview-abc.supaclank.dev.evil.example",
			wantOrigin:  "https://preview-abc.supaclank.dev.evil.example",
		},
		{
			name:        "a different port is a different origin",
			inboundHost: "127.0.0.1:53752",
			origin:      "http://127.0.0.1:9999",
			wantOrigin:  "http://127.0.0.1:9999",
		},
		{
			name:        "local preview translates its own loopback port",
			inboundHost: "127.0.0.1:53752",
			origin:      "http://127.0.0.1:53752",
			wantOrigin:  "http://127.0.0.1:53685",
		},
		{
			name:        "opaque origin names no host and is never translated",
			inboundHost: inbound,
			origin:      originOpaque,
			wantOrigin:  originOpaque,
		},
		{
			name:        "empty inbound host fails closed",
			inboundHost: "",
			origin:      "https://preview-abc.supaclank.dev",
			wantOrigin:  "https://preview-abc.supaclank.dev",
		},
		{
			name:        "referer keeps its path and query",
			inboundHost: inbound,
			referer:     "https://preview-abc.supaclank.dev/app/page?q=1",
			wantReferer: "http://127.0.0.1:53685/app/page?q=1",
		},
		{
			name:        "foreign referer is forwarded untouched",
			inboundHost: inbound,
			referer:     "https://evil.example/attack",
			wantReferer: "https://evil.example/attack",
		},
		{
			name:        "relative referer names no host",
			inboundHost: inbound,
			referer:     "/app/page",
			wantReferer: "/app/page",
		},
		{
			name:        "scheme-relative origin is not an absolute URL",
			inboundHost: inbound,
			origin:      "//preview-abc.supaclank.dev",
			wantOrigin:  "//preview-abc.supaclank.dev",
		},
		{
			name:        "scheme-relative referer is not an absolute URL",
			inboundHost: inbound,
			referer:     "//preview-abc.supaclank.dev/app/page",
			wantReferer: "//preview-abc.supaclank.dev/app/page",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/_next/static/chunks/x.js", nil)
			req.Header.Del(headerOrigin)
			if tc.origin != "" {
				req.Header.Set(headerOrigin, tc.origin)
			}
			if tc.referer != "" {
				req.Header.Set(headerReferer, tc.referer)
			}

			rewriteBrowserOrigin(req, tc.inboundHost, target)

			if got := req.Header.Get(headerOrigin); got != tc.wantOrigin {
				t.Errorf("Origin = %q, want %q", got, tc.wantOrigin)
			}
			if got := req.Header.Get(headerReferer); got != tc.wantReferer {
				t.Errorf("Referer = %q, want %q", got, tc.wantReferer)
			}
		})
	}
}

// crossOriginGateUpstream is the check every modern dev server ships —
// Next's blockCrossSiteDEV, Vite's origin token — reduced to its shared
// rule: an Origin naming any host other than the one this server is
// listening on is refused. Reproducing the rule (rather than asserting
// on headers) is what makes the proxy's translation observable the way
// a browser observes it.
func crossOriginGateUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get(headerOrigin); origin != "" {
			if u, err := url.Parse(origin); err != nil || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "Unauthorized", http.StatusForbidden)
				return
			}
		}
		io.WriteString(w, "chunk")
	})
}

// A CORS-mode same-origin subresource — Next tags two bootstrap chunks
// `crossorigin`, so the browser attaches Origin — used to reach the dev
// server carrying the preview host and get a 403, which aborted
// hydration and left client components dead on the page.
func TestProxyLetsSameOriginCORSSubresourceThrough(t *testing.T) {
	t.Parallel()
	s := startTestStack(t, crossOriginGateUpstream(), http.NotFoundHandler())

	req, err := http.NewRequest(http.MethodGet, s.URL+"/_next/static/chunks/web_1memeos._.js", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(headerOrigin, s.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET chunk: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200: the dev server still saw a foreign origin", resp.StatusCode, body)
	}
	if string(body) != "chunk" {
		t.Errorf("body = %q, want %q", body, "chunk")
	}
}

func TestProxyLeavesCrossSiteOriginForUpstreamToRefuse(t *testing.T) {
	t.Parallel()
	s := startTestStack(t, crossOriginGateUpstream(), http.NotFoundHandler())

	req, err := http.NewRequest(http.MethodGet, s.URL+"/_next/static/chunks/web_1memeos._.js", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(headerOrigin, "https://evil.example")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET chunk: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403: a foreign origin must reach upstream unchanged", resp.StatusCode)
	}
}

// HMR sockets carry Origin and WebSocket has no same-origin policy, so
// the dev server's Origin check is the only gate on the upgrade — and
// the only reason HMR died through a preview.
func TestProxyRewritesOriginOnWebSocketUpgrade(t *testing.T) {
	t.Parallel()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"X-Seen-Origin: %s\r\nX-Seen-Host: %s\r\n\r\n", r.Header.Get(headerOrigin), r.Host)
		buf.Flush()
	})
	s := startTestStack(t, upstream, http.NotFoundHandler())

	host := strings.TrimPrefix(s.URL, "http://")
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	fmt.Fprintf(conn, "GET /_next/hmr HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nOrigin: %s\r\n\r\n", host, s.URL)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	seenOrigin, seenHost := resp.Header.Get("X-Seen-Origin"), resp.Header.Get("X-Seen-Host")
	if want := "http://" + seenHost; seenOrigin != want {
		t.Errorf("upstream saw Origin %q, want %q — its own origin", seenOrigin, want)
	}
}
