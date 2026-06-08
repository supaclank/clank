package hostmux

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// captureBackend records the SendMessageOpts handed to OpenAndSend so a test
// can assert what the HTTP layer forwarded.
type captureBackend struct {
	mu      sync.Mutex
	gotPerm agent.ClaudePermissionMode
}

func (b *captureBackend) Open(context.Context) error { return nil }
func (b *captureBackend) OpenAndSend(_ context.Context, opts agent.SendMessageOpts) error {
	b.mu.Lock()
	b.gotPerm = opts.PermissionMode
	b.mu.Unlock()
	return nil
}
func (b *captureBackend) Send(context.Context, agent.SendMessageOpts) error { return nil }
func (b *captureBackend) Abort(context.Context) error                       { return nil }
func (b *captureBackend) Stop() error                                       { return nil }
func (b *captureBackend) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (b *captureBackend) Status() agent.SessionStatus                           { return agent.StatusIdle }
func (b *captureBackend) SessionID() string                                     { return "ext-stub" }
func (b *captureBackend) Messages(context.Context) ([]agent.MessageData, error) { return nil, nil }
func (b *captureBackend) Revert(context.Context, string) error                  { return nil }
func (b *captureBackend) Fork(context.Context, string) (agent.ForkResult, error) {
	return agent.ForkResult{}, nil
}
func (b *captureBackend) RespondPermission(context.Context, string, bool) error { return nil }

type captureBackendManager struct{ backend *captureBackend }

func (m *captureBackendManager) Init(context.Context, func() ([]string, error)) error { return nil }
func (m *captureBackendManager) CreateBackend(context.Context, agent.BackendInvocation) (agent.SessionBackend, error) {
	return m.backend, nil
}
func (m *captureBackendManager) Shutdown() {}

func initGitRepoMux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	run("git", "config", "commit.gpgsign", "false")
	run("git", "remote", "add", "origin", "git@github.com:acksell/clank.git")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "initial")
	return dir
}

// A new Claude session created over HTTP must carry the selected permission
// mode from StartRequest through to the backend's OpenAndSend.
func TestCreateSession_ClaudePermissionMode_ReachesBackend(t *testing.T) {
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
		Backend:        agent.BackendClaudeCode,
		GitRef:         agent.GitRef{LocalPath: dir},
		Prompt:         "hi",
		PermissionMode: agent.ClaudePermPlan,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", w.Code, w.Body.String())
	}

	backend.mu.Lock()
	got := backend.gotPerm
	backend.mu.Unlock()
	if got != agent.ClaudePermPlan {
		t.Errorf("backend received PermissionMode=%q, want plan", got)
	}
}
