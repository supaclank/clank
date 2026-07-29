package webpreview

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/presets"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/hosttest"
	hostmux "github.com/acksell/clank/internal/host/mux"
	hoststore "github.com/acksell/clank/internal/host/store"
)

// TestOverlayCreateFlow_DefaultPresetSatisfiesHostValidation replays the
// overlay's session-create wire sequence (overlay.js send()) against a
// real host behind the daemon relay: GET /presets, apply the backend's
// Default preset config verbatim, POST /sessions. Regression for the
// overlay creating config-less sessions, which the host rejects with
// 400 config_incomplete (it never fills values in).
func TestOverlayCreateFlow_DefaultPresetSatisfiesHostValidation(t *testing.T) {
	t.Parallel()

	stub := &hosttest.StubBackendManager{}
	hs, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("hoststore.Open: %v", err)
	}
	t.Cleanup(func() { hs.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendClaudeCode: stub,
		},
		SessionsStore: hs,
		// No BuiltinPresets on purpose: host.New resolves the Workstation
		// set, the same default a hand-started clank-host runs with.
	})
	t.Cleanup(svc.Shutdown)

	s := startTestStack(t, http.NotFoundHandler(), hostmux.New(svc, nil).Handler())
	repo := hosttest.InitGitRepo(t)

	do := func(method, path, body string) (*http.Response, string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, s.URL+"/__clank/api"+path, rd)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer sekrit")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	// Step 1: the overlay's preset fetch (same query overlay.js sends).
	resp, body := do(http.MethodGet, "/presets?backend=claude-code&hostname=local", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /presets: %d %s", resp.StatusCode, body)
	}
	var ps []presets.Preset
	if err := json.Unmarshal([]byte(body), &ps); err != nil {
		t.Fatalf("decode presets: %v", err)
	}
	def := presets.DefaultFor(ps, agent.BackendClaudeCode)
	if def == nil {
		t.Fatalf("no Default preset served for claude-code: %s", body)
	}
	cfgJSON, _ := json.Marshal(def.Config)

	// Step 2: the overlay's create — raw JSON in the exact field shape
	// overlay.js posts, config applied verbatim from the preset.
	createBody := fmt.Sprintf(
		`{"backend":"claude-code","hostname":"local","git_ref":{"local_path":%q},"prompt":"make the header sticky","config":%s}`,
		repo, cfgJSON)
	resp, body = do(http.MethodPost, "/sessions", createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /sessions: %d %s (config-less create regression?)", resp.StatusCode, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == "" {
		t.Fatalf("decode created session: err=%v body=%s", err, body)
	}

	// The preset bundle reached the backend untouched.
	last := stub.Last()
	if last == nil {
		t.Fatal("no backend created")
	}
	got := last.LastSendOpts().Config
	for k, v := range def.Config {
		if got[k] != v {
			t.Errorf("backend config[%q] = %q, want %q (Default preset value)", k, got[k], v)
		}
	}
}
