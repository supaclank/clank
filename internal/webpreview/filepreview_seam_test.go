package webpreview

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/filepreview"
)

// TestProxyFrontsFilePreview pins the `clank preview <file>` seam: the
// filepreview text shell rides through the overlay proxy like any dev
// server and comes out carrying both the escaped file content and the
// injected overlay — the file page is a first-class overlay surface.
func TestProxyFrontsFilePreview(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello <world>"), 0o644); err != nil {
		t.Fatal(err)
	}
	fh, err := filepreview.NewHandler(filepreview.Options{Root: dir, Entry: "notes.txt"})
	if err != nil {
		t.Fatalf("filepreview handler: %v", err)
	}
	t.Cleanup(fh.Close)

	s := startTestStack(t, fh, http.NotFoundHandler())

	// "/" exercises the entry redirect through the proxy on the way.
	resp, err := http.Get(s.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Request.URL.Path; got != "/notes.txt" {
		t.Fatalf("landed on %q, want /notes.txt (entry redirect)", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)
	if !strings.Contains(page, "hello &lt;world&gt;") {
		t.Errorf("escaped file content missing:\n%s", page)
	}
	if !strings.Contains(page, `src="/__clank/overlay.js"`) {
		t.Errorf("overlay script not injected into the file page:\n%s", page)
	}
	if !strings.Contains(page, "<pre") {
		t.Errorf("text shell <pre> missing:\n%s", page)
	}
}
