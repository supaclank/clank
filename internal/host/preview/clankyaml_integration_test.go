//go:build unix

package preview

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pyServerBody is a minimal real HTTP server for clank.yaml fixtures:
// binds 127.0.0.1 on the port given as argv[1], answers every GET with
// pyServerReply. Same threading-before-serve shape as fakeMetroBody so
// the readiness probe can't race the listener.
const pyServerReply = "clank-custom-ok"

const pyServerBody = `import http.server, sys, threading, time
port = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Type', 'text/plain')
        self.end_headers()
        self.wfile.write(b'` + pyServerReply + `')
    def log_message(self, *args): pass
srv = http.server.HTTPServer(('127.0.0.1', port), H)
threading.Thread(target=srv.serve_forever, daemon=True).start()
while True: time.sleep(60)
`

func requirePython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
}

// waitForReadyStatus polls Manager.Status until the record reports
// Ready, failing on Failed (with the log tail for diagnosis) or on
// timeout.
func waitForReadyStatus(t *testing.T, m *Manager, wid, workDir string, timeout time.Duration) Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st, err := m.Status(context.Background(), wid, workDir)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		switch st.State {
		case StateReady:
			return st
		case StateFailed:
			t.Fatalf("preview failed: %s\nlogs:\n%s", st.LastErr, m.LogTail(wid))
		}
		if time.Now().After(deadline) {
			t.Fatalf("preview did not become ready within %s (state=%s)\nlogs:\n%s", timeout, st.State, m.LogTail(wid))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestManagerStart_ClankYAMLCustomCommand is the end-to-end contract
// for the clank.yaml escape hatch: a project with NO detectable
// framework, whose clank.yaml declares a real dev-server command in a
// subdirectory, previews through the full production path —
// Manager.Start → Detect → spawn → readiness probe — and serves HTTP
// on the allocated port. The command's ${PORT} is substituted and the
// relative server.py only resolves if the spawn actually ran in
// preview.dir.
func TestManagerStart_ClankYAMLCustomCommand(t *testing.T) {
	t.Parallel()
	requirePython3(t)

	workDir := t.TempDir()
	writeTree(t, workDir, map[string]string{
		".git/": "",
		"clank.yaml": "preview:\n" +
			"  dir: site\n" +
			"  command: python3 -u server.py ${PORT}\n" +
			"  ready:\n" +
			"    path: /\n" +
			"    expect: " + pyServerReply + "\n",
		"site/server.py": pyServerBody,
	})

	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	const wid = "wt-clankyaml-custom"
	st, err := m.Start(context.Background(), wid, workDir, "default")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.Kind != KindWeb {
		t.Errorf("Kind = %q, want %q (custom commands are the browser flow)", st.Kind, KindWeb)
	}
	st = waitForReadyStatus(t, m, wid, workDir, 15*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", st.Port))
	if err != nil {
		t.Fatalf("GET preview: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), pyServerReply) {
		t.Errorf("preview served %q, want %q", body, pyServerReply)
	}
}

// TestManagerStart_ClankYAMLInstallRunsOnceBehindMarker covers the
// optional install step for custom commands: it runs before the
// server, its completion marker records the verbatim install command,
// and a restart with a warm marker re-runs it (installs are
// reconciling no-ops for real package managers — the marker exists to
// scope the wipe decision, not to skip installs). Not parallel:
// CLANK_DIR pins the marker location.
func TestManagerStart_ClankYAMLInstallRunsOnceBehindMarker(t *testing.T) {
	requirePython3(t)
	t.Setenv("CLANK_DIR", t.TempDir())

	workDir := t.TempDir()
	installCmd := "sh install.sh"
	writeTree(t, workDir, map[string]string{
		".git/": "",
		"clank.yaml": "preview:\n" +
			"  install: " + installCmd + "\n" +
			"  command: python3 -u server.py ${PORT}\n" +
			"  ready:\n" +
			"    path: /\n" +
			"    expect: " + pyServerReply + "\n",
		"server.py":  pyServerBody,
		"install.sh": "echo run >> installed.txt\n",
	})

	m := New(Options{StopGrace: 1 * time.Second})
	defer m.Shutdown()

	const wid = "wt-clankyaml-install"
	if _, err := m.Start(context.Background(), wid, workDir, "default"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForReadyStatus(t, m, wid, workDir, 15*time.Second)

	if _, err := os.Stat(filepath.Join(workDir, "installed.txt")); err != nil {
		t.Fatalf("install step did not run: %v", err)
	}
	markerPath, err := bootstrapMarkerPath(workDir)
	if err != nil {
		t.Fatalf("bootstrapMarkerPath: %v", err)
	}
	content, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("completion marker not written: %v", err)
	}
	if got := strings.TrimSpace(string(content)); got != installCmd {
		t.Errorf("marker content = %q, want the verbatim install command %q", got, installCmd)
	}
	if _, err := os.Stat(markerPath + markerInstallingSuffix); !os.IsNotExist(err) {
		t.Errorf("in-flight sentinel should be cleared after a successful install, stat err = %v", err)
	}
}
