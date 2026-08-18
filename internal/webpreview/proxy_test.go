package webpreview

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// spyReadCloser records whether Close was called, so tests can assert
// readUpTo releases the upstream body instead of leaking the connection.
type spyReadCloser struct {
	io.Reader
	closed bool
}

func (s *spyReadCloser) Close() error {
	s.closed = true
	return nil
}

func TestReadUpToClosesBodyWhenFullyBuffered(t *testing.T) {
	t.Parallel()
	rc := &spyReadCloser{Reader: strings.NewReader("hello")}
	body, overflow, err := readUpTo(rc, 100)
	if err != nil {
		t.Fatalf("readUpTo: %v", err)
	}
	if overflow != nil {
		t.Fatalf("want no overflow reader for a body under the limit")
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want %q", body, "hello")
	}
	if !rc.closed {
		t.Errorf("original body must be closed once fully buffered, to release the upstream connection")
	}
}

func TestReadUpToClosesBodyOnReadError(t *testing.T) {
	t.Parallel()
	rc := &spyReadCloser{Reader: iotest.ErrReader(errors.New("boom"))}
	if _, _, err := readUpTo(rc, 100); err == nil {
		t.Fatalf("want a read error")
	}
	if !rc.closed {
		t.Errorf("original body must be closed on read error")
	}
}

func TestReadUpToLeavesOverflowBodyOpenUntilCallerCloses(t *testing.T) {
	t.Parallel()
	rc := &spyReadCloser{Reader: strings.NewReader(strings.Repeat("x", 50))}
	body, overflow, err := readUpTo(rc, 10)
	if err != nil {
		t.Fatalf("readUpTo: %v", err)
	}
	if body != nil {
		t.Fatalf("want nil body when overflowing the limit")
	}
	if overflow == nil {
		t.Fatalf("want an overflow reader")
	}
	if rc.closed {
		t.Fatalf("must not close the body while the overflow reader still wraps it")
	}
	if _, err := io.ReadAll(overflow); err != nil {
		t.Fatalf("drain overflow: %v", err)
	}
	if err := overflow.Close(); err != nil {
		t.Fatalf("close overflow: %v", err)
	}
	if !rc.closed {
		t.Errorf("closing the overflow reader must close the underlying body")
	}
}

// startTestStack wires a fake Vite upstream and a fake daemon on a unix
// socket behind a real proxy Server, mirroring production topology.
func startTestStack(t *testing.T, upstream http.Handler, daemon http.Handler) *Server {
	t.Helper()
	return startTestStackOpts(t, upstream, daemon, nil)
}

// startTestStackOpts is startTestStack with an Options hook applied
// just before Start, for tests exercising non-default options.
func startTestStackOpts(t *testing.T, upstream http.Handler, daemon http.Handler, mutate func(*Options)) *Server {
	t.Helper()

	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)

	// Not t.TempDir(): its path embeds the test name and blows through
	// macOS's 104-byte sockaddr_un limit.
	sockDir, err := os.MkdirTemp("", "wp")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("unix listen: %v", err)
	}
	dsrv := &http.Server{Handler: daemon}
	go dsrv.Serve(ln)
	t.Cleanup(func() { dsrv.Close() })

	opts := Options{
		UpstreamURL:      u,
		DaemonSocketPath: sock,
		Token:            "sekrit",
		OverlayConfig:    map[string]any{"name": "app", "local_path": "/tmp/app"},
	}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := Start(opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s
}

func TestProxyInjectsOverlayIntoHTML(t *testing.T) {
	t.Parallel()
	s := startTestStack(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'self'")
			io.WriteString(w, "<html><head><title>x</title></head><body>app</body></html>")
		}),
		http.NotFoundHandler(),
	)

	resp, err := http.Get(s.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	for _, want := range []string{
		`window.__CLANK_PREVIEW = `,
		`"token":"sekrit"`,
		`"local_path":"/tmp/app"`,
		`src="/__clank/overlay.js"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("injected HTML missing %q\nbody: %s", want, body)
		}
	}
	if idx := strings.Index(string(body), "</head>"); idx < 0 || !strings.Contains(string(body[:idx]), "__CLANK_PREVIEW") {
		t.Errorf("config script must land inside <head>")
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		t.Errorf("CSP must be dropped on injected pages, got %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("injected HTML must be no-store (it embeds the per-run token), got %q", got)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length %s != body %d", cl, len(body))
	}
}

// The Rewrite hook asks upstream for identity encoding, but an upstream
// that ignores Accept-Encoding and compresses anyway must not have its
// compressed bytes searched (and corrupted) by injectHTML.
func TestProxyLeavesCompressedHTMLAlone(t *testing.T) {
	t.Parallel()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, "<html><head></head><body>app</body></html>")
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	compressed := gz.Bytes()

	s := startTestStack(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(compressed)
		}),
		http.NotFoundHandler(),
	)

	// DisableCompression so this client's own Transport doesn't transparently
	// gunzip the response — the point is to inspect exactly what the proxy
	// sent over the wire.
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Get(s.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, compressed) {
		t.Fatalf("compressed body was modified: got %d bytes, want the original %d untouched", len(body), len(compressed))
	}
}

func TestProxyLeavesNonHTMLAlone(t *testing.T) {
	t.Parallel()
	const js = "export const x = 1;"
	s := startTestStack(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript")
			io.WriteString(w, js)
		}),
		http.NotFoundHandler(),
	)
	resp, err := http.Get(s.URL + "/src/app.js")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != js {
		t.Fatalf("non-HTML body modified: %q", body)
	}
}

func TestDaemonRelayRequiresTokenAndStripsAuth(t *testing.T) {
	t.Parallel()
	var gotPath, gotAuth string
	s := startTestStack(t,
		http.NotFoundHandler(),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNoContent)
		}),
	)

	// No token → 401, daemon never sees it.
	resp, err := http.Get(s.URL + "/__clank/api/sessions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tokenless status = %d, want 401", resp.StatusCode)
	}

	// Bearer token → relayed with the prefix stripped and auth removed.
	req, _ := http.NewRequest("GET", s.URL+"/__clank/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if gotPath != "/sessions" {
		t.Errorf("daemon path = %q, want /sessions (prefix stripped)", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("Authorization must not reach the daemon, got %q", gotAuth)
	}
}

// TestRequireTokenRejectsOversizedTokenWithoutMatching pins the length
// short-circuit: a wrong-length token must be rejected without ever
// running the constant-time byte comparison over it, so a client can't
// force large per-request allocations by sending an oversized token.
func TestRequireTokenRejectsOversizedTokenWithoutMatching(t *testing.T) {
	t.Parallel()
	s := startTestStack(t, http.NotFoundHandler(), http.NotFoundHandler())

	huge := strings.Repeat("a", 512<<10) // 512 KiB, far longer than any real token but under the server's header-size cap
	req, _ := http.NewRequest("GET", s.URL+"/__clank/api/sessions", nil)
	req.Header.Set("X-Clank-Token", huge)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("oversized token status = %d, want 401", resp.StatusCode)
	}
}

func TestOverlayAssetsServed(t *testing.T) {
	t.Parallel()
	s := startTestStack(t, http.NotFoundHandler(), http.NotFoundHandler())
	for _, path := range []string{"/__clank/overlay.js", "/__clank/chat.js", "/__clank/settings.js", "/__clank/worklet.js", "/__clank/boxpos.js", "/__clank/launcher.js"} {
		resp, err := http.Get(s.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Errorf("%s: status %d, %d bytes", path, resp.StatusCode, len(body))
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("%s content-type = %q", path, ct)
		}
	}
}
