package hostmux_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostmux "github.com/acksell/clank/internal/host/mux"
)

// TestSoftwareManifestEndpoint_ReturnsOpenCodeVersion smoke-tests
// the new /software-manifest endpoint against a real opencode binary
// and confirms the shape that clankcli's compat check depends on.
func TestSoftwareManifestEndpoint_ReturnsOpenCodeVersion(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on $PATH")
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	mux := hostmux.New(svc, nil)
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/software-manifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got agent.SoftwareManifest
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.OpenCode.Version == "" {
		t.Errorf("OpenCode.Version is empty; real opencode binary should yield a version")
	}
	if got.OpenCode.Version[0] < '0' || got.OpenCode.Version[0] > '9' {
		t.Errorf("OpenCode.Version %q doesn't start with a digit", got.OpenCode.Version)
	}
}

// TestSoftwareManifestEndpoint_SubsequentCallsAreFast pins the
// caching contract: only the first call pays opencode's JS startup
// cost. The second call must return in well under what a fresh
// `opencode --version` subprocess would take (~200-500ms).
func TestSoftwareManifestEndpoint_SubsequentCallsAreFast(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not on $PATH")
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	mux := hostmux.New(svc, nil)
	srv := httptest.NewServer(mux.Handler())
	t.Cleanup(srv.Close)

	// Prime the cache (process-global sync.Once).
	resp, err := http.Get(srv.URL + "/software-manifest")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Second call should be in-memory only. Generous ceiling (50ms)
	// to absorb HTTP overhead + JSON encode + httptest plumbing
	// without making the test flaky on slow CI. Catches the
	// regression where someone removes the cache and falls back to
	// re-shelling opencode every request — opencode's startup
	// alone is ~200ms+, so we'd fail this hard.
	start := time.Now()
	resp, err = http.Get(srv.URL + "/software-manifest")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if elapsed > 50*time.Millisecond {
		t.Errorf("second /software-manifest call took %v; expected <50ms (cache regression?)", elapsed)
	}
}
