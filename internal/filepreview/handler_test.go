package filepreview

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestServer stages files (slash-relative paths) under a temp root
// and runs a real Server against it — production topology, no recorder.
func newTestServer(t *testing.T, entry string, files map[string]string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := Start(Options{Root: dir, Entry: entry, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return srv, dir
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

func TestIndexRedirectsToEntry(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "my notes.md", map[string]string{"my notes.md": "hello"})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	// Path-encoded, so entries with spaces/unicode survive the Location.
	if loc := resp.Header.Get("Location"); loc != "/my%20notes.md" {
		t.Fatalf("Location = %q, want /my%%20notes.md", loc)
	}
}

// TestTextShellEscapesContent is the shell's XSS regression test: file
// bytes must render as text, never execute — a previewed file (or a
// hostile page a shared preview serves later) must not script against
// the origin that also holds the overlay.
func TestTextShellEscapesContent(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "evil.txt", map[string]string{"evil.txt": `<script>alert(1)</script>`})
	resp, body := get(t, srv.URL+"/evil.txt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html (the overlay proxy only injects into HTML)", ct)
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("file content served unescaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("escaped file content missing:\n%s", body)
	}
	if !strings.Contains(body, "<pre") || !strings.Contains(body, routeReload) {
		t.Fatalf("shell must carry the <pre> view and the reload client:\n%s", body)
	}
}

func TestHTMLServedRaw(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "page.html", map[string]string{"page.html": "<h1>hi</h1>"})
	resp, body := get(t, srv.URL+"/page.html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if body != "<h1>hi</h1>" {
		t.Fatalf("html must pass through untouched, got:\n%s", body)
	}
}

func TestBinaryMIMEPassthrough(t *testing.T) {
	t.Parallel()
	png := "\x89PNG\r\n\x1a\n0000"
	srv, _ := newTestServer(t, "a.txt", map[string]string{"a.txt": "x", "img.png": png})
	resp, body := get(t, srv.URL+"/img.png")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Fatalf("Content-Type = %q, want image/png", ct)
	}
	if body != png {
		t.Fatalf("binary must pass through untouched, got %q", body)
	}
}

func TestNULFileServedAsDownload(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "a.txt", map[string]string{"a.txt": "x", "blob": "abc\x00def"})
	resp, _ := get(t, srv.URL+"/blob")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream (no garbage text shell)", ct)
	}
}

func TestMissingFileNotFound(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "a.txt", map[string]string{"a.txt": "x"})
	if resp, _ := get(t, srv.URL+"/nope.txt"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDirectoryNotFound(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "a.txt", map[string]string{"a.txt": "x", "sub/b.txt": "y"})
	if resp, _ := get(t, srv.URL+"/sub"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestSymlinkEscapeNotFound pins the os.Root containment choice: a
// symlink inside the project pointing out of it must not leak the
// target — plain Clean/HasPrefix containment would have followed it.
func TestSymlinkEscapeNotFound(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, root := newTestServer(t, "a.txt", map[string]string{"a.txt": "x"})
	if err := os.Symlink(secret, filepath.Join(root, "abs-link.txt")); err != nil {
		t.Fatal(err)
	}
	relTarget, err := filepath.Rel(root, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relTarget, filepath.Join(root, "rel-link.txt")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/abs-link.txt", "/rel-link.txt", "/../secret.txt"} {
		if resp, body := get(t, srv.URL+p); resp.StatusCode != http.StatusNotFound || strings.Contains(body, "s3cret") {
			t.Fatalf("GET %s: status = %d body = %q, want 404 without the secret", p, resp.StatusCode, body)
		}
	}
}

// TestForeignHostForbidden pins the DNS-rebinding defense: a hostile
// page can point its own domain at 127.0.0.1 and become same-origin
// with this server — the Host header is the tell.
func TestForeignHostForbidden(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "a.txt", map[string]string{"a.txt": "x"})
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/a.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestOversizeFileRejected(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, "big.txt", map[string]string{"big.txt": strings.Repeat("a", maxFileBytes+1)})
	if resp, _ := get(t, srv.URL+"/big.txt"); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestLoopbackHost(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]bool{
		"127.0.0.1:5001":            true,
		"127.0.0.1":                 true,
		"localhost:80":              true,
		"localhost":                 true,
		"[::1]:8080":                true,
		"::1":                       true,
		"evil.example":              false,
		"127.0.0.1.evil.example:80": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
