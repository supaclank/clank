package hostmux

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// configOptionsBackendManager fakes an ACP manager whose on-demand probe
// returns a fixed option set, recording the dir the service resolved.
type configOptionsBackendManager struct {
	captureBackendManager
	opts    []agent.ConfigOption
	lastDir string
}

func (m *configOptionsBackendManager) ConfigOptions(_ context.Context, projectDir string) ([]agent.ConfigOption, error) {
	m.lastDir = projectDir
	return m.opts, nil
}

// GET /config-options resolves the GitRef to a workdir host-side (paths
// never cross the wire beyond the caller-supplied LocalPath) and serves
// the manager's live probe verbatim; a missing backend is a 400.
func TestConfigOptionsOverHTTP(t *testing.T) {
	dir := initGitRepoMux(t)
	fake := &configOptionsBackendManager{
		captureBackendManager: captureBackendManager{backend: &captureBackend{}},
		opts: []agent.ConfigOption{{
			ID: "mode", Name: "Mode", Category: agent.ConfigOptionMode, CurrentValue: "agent",
			Values: []agent.ConfigOptionValue{
				{Value: "read-only", Name: "Read Only"},
				{Value: "agent", Name: "Agent"},
			},
		}},
	}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendCodex: fake,
		},
		PresetsDir: t.TempDir(),
	})
	t.Cleanup(svc.Shutdown)
	m := New(svc, nil)

	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/config-options?backend=codex&git_local_path="+url.QueryEscape(dir), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /config-options: %d %s", w.Code, w.Body.String())
	}
	var out []agent.ConfigOption
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].ID != "mode" || len(out[0].Values) != 2 {
		t.Fatalf("options = %+v, want the manager's probe verbatim", out)
	}
	if fake.lastDir == "" {
		t.Fatal("service never resolved the ref to a workdir for the probe")
	}

	w = httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/config-options?git_local_path="+url.QueryEscape(dir), nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET without backend = %d, want 400", w.Code)
	}
}
