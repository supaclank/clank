package hostmux

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/presets"
	"github.com/acksell/clank/internal/host"
)

func presetsService(t *testing.T) *host.Service {
	t.Helper()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendClaudeCode: &captureBackendManager{backend: &captureBackend{}},
		},
		BuiltinPresets: presets.Sandbox(),
		PresetsDir:     t.TempDir(),
	})
	t.Cleanup(svc.Shutdown)
	return svc
}

// GET serves built-ins (provisioner-declared) plus user presets; POST and
// DELETE manage the user rows; built-in ids are write-protected.
func TestPresets_CRUDOverHTTP(t *testing.T) {
	m := New(presetsService(t), nil)

	get := func() []presets.Preset {
		t.Helper()
		w := httptest.NewRecorder()
		m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/presets?backend=claude-code", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /presets: %d %s", w.Code, w.Body.String())
		}
		var out []presets.Preset
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	base := get()
	if len(base) != 2 || !base[0].Builtin || !base[1].Builtin {
		t.Fatalf("expected the two claude built-ins first, got %+v", base)
	}

	user := presets.Preset{
		ID: "review", Name: "Review", Backend: agent.BackendClaudeCode,
		Config: map[string]string{agent.ConfigOptionMode: "plan", "model": "default", "effort": "max"},
	}
	body, _ := json.Marshal(user)
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/presets", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /presets: %d %s", w.Code, w.Body.String())
	}
	if got := get(); len(got) != 3 || got[2].ID != "review" {
		t.Fatalf("after POST: %+v", got)
	}

	// Built-in ids are reserved.
	bad := user
	bad.ID = presets.BuiltinDefaultPrefix + "claude-code"
	body, _ = json.Marshal(bad)
	w = httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/presets", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST over builtin id: %d, want 400", w.Code)
	}

	w = httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/presets/review", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	if got := get(); len(got) != 2 {
		t.Fatalf("after DELETE: %+v", got)
	}
}

// A create whose config is missing the backend's required keys (the
// Default preset's keys) fails with 400 and names every gap — the host
// never fills values in.
func TestCreateSession_RejectsIncompleteConfig(t *testing.T) {
	m := New(presetsService(t), nil)

	dir := initGitRepoMux(t)
	body, _ := json.Marshal(agent.StartRequest{
		Backend: agent.BackendClaudeCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
		Config:  map[string]string{agent.ConfigOptionMode: "plan"}, // model+effort missing
	})
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", w.Code, w.Body.String())
	}
	if b := w.Body.String(); !strings.Contains(b, "model") || !strings.Contains(b, "effort") || !strings.Contains(b, "config_incomplete") {
		t.Errorf("400 must carry the code and name every missing key, got: %s", b)
	}

	// The complete config passes.
	body, _ = json.Marshal(agent.StartRequest{
		Backend: agent.BackendClaudeCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
		Config: map[string]string{
			agent.ConfigOptionMode: "bypassPermissions", "model": "default", "effort": "default",
		},
	})
	w = httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("complete config: status=%d body=%s, want 201", w.Code, w.Body.String())
	}
}
