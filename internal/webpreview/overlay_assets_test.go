package webpreview

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const overlaySourceDir = "overlay"

// relativeImport matches the `from './x.js'` specifiers overlay.js uses to
// pull in its pure modules. The browser resolves each against the injected
// script's own URL, so every one has to exist under /__clank/.
var relativeImport = regexp.MustCompile(`from '\./([A-Za-z0-9_.-]+\.js)'`)

func serveOverlayPath(t *testing.T, path string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	if !ServeOverlayAsset(rec, httptest.NewRequest(http.MethodGet, path, nil)) {
		return nil
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s served with status %d", path, rec.Code)
	}
	return rec.Body.Bytes()
}

// Every overlay module is embedded and routed by hand, so adding one and
// forgetting either step ships an overlay whose first import 404s — the
// whole module graph fails and nothing renders at all.
func TestEveryOverlayModuleIsServed(t *testing.T) {
	t.Parallel()
	names := overlayModuleFilenames(t)
	for _, name := range names {
		onDisk, err := os.ReadFile(filepath.Join(overlaySourceDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		served := serveOverlayPath(t, "/__clank/"+name)
		if served == nil {
			t.Errorf("overlay/%s is not served: add its //go:embed in proxy.go and its entry to overlayModules", name)
			continue
		}
		if string(served) != string(onDisk) {
			t.Errorf("/__clank/%s serves different bytes than overlay/%s", name, name)
		}
	}
}

// overlayModuleFilenames lists the browser-facing modules on disk. *_test.mjs
// is node-only and never reaches the browser.
func overlayModuleFilenames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(overlaySourceDir)
	if err != nil {
		t.Fatalf("read overlay source dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no overlay modules found; the source dir moved")
	}
	return names
}

// The path clank preview actually uses: the running proxy's own mux, which
// serves from the same overlayModules table as the gateway.
func TestRunningProxyServesEveryOverlayModule(t *testing.T) {
	t.Parallel()
	s := startTestStack(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html></html>")) }),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, name := range overlayModuleFilenames(t) {
		res, err := http.Get(s.URL + "/__clank/" + name)
		if err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("the running proxy serves /__clank/%s as %d; the overlay's import of it would fail", name, res.StatusCode)
			continue
		}
		// Unrouted /__clank paths fall through to the upstream app, which
		// answers 200 with HTML — so status alone proves nothing. The body
		// has to be the module itself.
		onDisk, err := os.ReadFile(filepath.Join(overlaySourceDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(body) != string(onDisk) {
			t.Errorf("the running proxy does not serve overlay/%s; the request fell through to the app", name)
		}
	}
}

// The import graph reached from overlay.js has to be servable end to end:
// a specifier pointing at a module the proxy doesn't route breaks the
// overlay just as completely as a missing file.
func TestOverlayImportsResolveToServedModules(t *testing.T) {
	t.Parallel()
	pending := []string{"overlay.js"}
	visited := map[string]bool{}
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		if visited[name] {
			continue
		}
		visited[name] = true
		body := serveOverlayPath(t, "/__clank/"+name)
		if body == nil {
			t.Errorf("%s is imported by the overlay but not served", name)
			continue
		}
		for _, m := range relativeImport.FindAllSubmatch(body, -1) {
			pending = append(pending, string(m[1]))
		}
	}
	// The bug this guards: toplayer.js was imported by overlay.js while
	// being neither embedded nor routed, so the overlay never appeared.
	if !visited["toplayer.js"] {
		t.Error("overlay.js must import the top-layer policy module")
	}
}
