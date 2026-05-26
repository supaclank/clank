package preview

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestReverseProxyStripsSensitiveHeaders is the regression test for
// Change 3 of the plan. The dev server (Metro, Vite, …) is running user
// code; it must never see the sprite bearer or any clank session
// cookies. Same defense pkg/gateway/remote_proxy.go applies one hop
// earlier.
func TestReverseProxyStripsSensitiveHeaders(t *testing.T) {
	t.Parallel()
	// Backend records every request header it sees.
	seen := make(http.Header)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range r.Header {
			seen[k] = v
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()

	port := portFromTestServer(t, backend.URL)
	proxy, err := newReverseProxy(port, "http://example.test/preview")
	if err != nil {
		t.Fatalf("newReverseProxy: %v", err)
	}

	front := httptest.NewServer(proxy)
	defer front.Close()

	req, err := http.NewRequest("GET", front.URL+"/index.bundle", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret-sprite-token")
	req.Header.Set("Cookie", "session=secret-session")
	req.Header.Set("X-Forwarded-Host", "spoofed.example.com")
	req.Header.Set("X-Custom-Keep", "should-pass-through")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if v := seen.Get("Authorization"); v != "" {
		t.Errorf("backend saw Authorization header %q; want it stripped", v)
	}
	if v := seen.Get("Cookie"); v != "" {
		t.Errorf("backend saw Cookie header %q; want it stripped", v)
	}
	if v := seen.Get("X-Forwarded-Host"); v != "" {
		t.Errorf("backend saw X-Forwarded-Host %q; want it stripped", v)
	}
	if v := seen.Get("X-Custom-Keep"); v != "should-pass-through" {
		t.Errorf("backend X-Custom-Keep = %q; want non-stripped headers preserved", v)
	}
}

// TestReverseProxyRewritesManifestURLs confirms the ModifyResponse
// hook prepends the proxy's path prefix to URLs Metro emits with the
// public origin. Regression test for the "manifest launchAsset.url
// points at /node_modules/... with no preview prefix" bug found
// during smoke-testing against Expo SDK 55.
func TestReverseProxyRewritesManifestURLs(t *testing.T) {
	t.Parallel()
	// Upstream emits a manifest with two URLs at the public origin —
	// one already-prefixed (idempotent path), one bare (needs rewrite).
	publicURL := "http://gateway.test:8080/worktrees/wt-abc/preview/proxy"
	manifest := `{"launchAsset":{"url":"http://gateway.test:8080/node_modules/foo/bundle"},` +
		`"already":{"url":"http://gateway.test:8080/worktrees/wt-abc/preview/proxy/already/asset"}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, manifest)
	}))
	defer backend.Close()

	port := portFromTestServer(t, backend.URL)
	proxy, err := newReverseProxy(port, publicURL)
	if err != nil {
		t.Fatalf("newReverseProxy: %v", err)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	wantPrefixed := "http://gateway.test:8080/worktrees/wt-abc/preview/proxy/node_modules/foo/bundle"
	if !strings.Contains(got, wantPrefixed) {
		t.Errorf("body missing rewritten URL %q\nfull body: %s", wantPrefixed, got)
	}
	wantUntouched := "http://gateway.test:8080/worktrees/wt-abc/preview/proxy/already/asset"
	if !strings.Contains(got, wantUntouched) {
		t.Errorf("already-prefixed URL was modified or dropped: %s", got)
	}
	// And the doubly-prefixed pathological URL must NOT appear:
	doublyPrefixed := "http://gateway.test:8080/worktrees/wt-abc/preview/proxy/worktrees/wt-abc/preview/proxy/"
	if strings.Contains(got, doublyPrefixed) {
		t.Errorf("URL was double-prefixed: %s", got)
	}
}

// TestReverseProxySkipsNonJSON confirms the URL rewriter does NOT
// touch binary or non-JSON payloads (bundles, source maps, images).
// Without this guard a stream-of-bytes asset would be loaded fully
// into memory and corrupted by chance substring matches.
func TestReverseProxySkipsNonJSON(t *testing.T) {
	t.Parallel()
	publicURL := "http://gateway.test:8080/worktrees/wt-abc/preview/proxy"
	bundle := []byte("// stuff at http://gateway.test:8080/node_modules/foo loaded here // EOF")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write(bundle)
	}))
	defer backend.Close()

	port := portFromTestServer(t, backend.URL)
	proxy, err := newReverseProxy(port, publicURL)
	if err != nil {
		t.Fatalf("newReverseProxy: %v", err)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	resp, err := http.Get(front.URL + "/index.bundle")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, bundle) {
		t.Errorf("non-JSON body was modified.\nwant: %q\ngot:  %q", bundle, body)
	}
}

// TestReverseProxyForwardsBodyAndPath confirms the happy-path proxy
// behaviour — anything we're not deliberately rewriting needs to
// survive both hops.
func TestReverseProxyForwardsBodyAndPath(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "path="+r.URL.Path+";method="+r.Method)
	}))
	defer backend.Close()

	port := portFromTestServer(t, backend.URL)
	proxy, err := newReverseProxy(port, "http://example.test/preview")
	if err != nil {
		t.Fatalf("newReverseProxy: %v", err)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	resp, err := http.Post(front.URL+"/foo/bar?x=1", "text/plain", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got, want := string(body), "path=/foo/bar;method=POST"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// portFromTestServer is a small helper: httptest.Server.URL is
// "http://127.0.0.1:<port>", but newReverseProxy wants just the port.
func portFromTestServer(t *testing.T, srvURL string) int {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatalf("parse %s: %v", srvURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port %q: %v", u.Port(), err)
	}
	return port
}
