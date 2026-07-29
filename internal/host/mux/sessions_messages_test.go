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

// GET /sessions/{id}/messages must serialize an empty history as `[]`,
// not `null` — clients decode the body as MessageData[].
func TestGetMessages_EmptyHistorySerializesAsEmptyArray(t *testing.T) {
	backend := &captureBackend{}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendClaudeCode: &captureBackendManager{backend: backend},
		},
	})
	t.Cleanup(svc.Shutdown)
	m := New(svc, nil)

	dir := initGitRepoMux(t)
	body, err := json.Marshal(agent.StartRequest{
		Backend: agent.BackendClaudeCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
		Config:  presets.DefaultFor(presets.Workstation(), agent.BackendClaudeCode).Config,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s, want 201", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("decode created session id: err=%v body=%s", err, w.Body.String())
	}

	// captureBackend.Messages returns (nil, nil) — the exact shape a fresh
	// or transcript-empty session produces.
	rw := httptest.NewRecorder()
	m.Handler().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID+"/messages", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("messages status=%d body=%s, want 200", rw.Code, rw.Body.String())
	}
	if got := strings.TrimSpace(rw.Body.String()); got != "[]" {
		t.Errorf("messages body = %q, want %q", got, "[]")
	}
}
