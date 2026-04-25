package host_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// noopBackendManager is a fixture (not a mock) used to construct a
// host.Service in tests that exercise non-backend code paths
// (CreateSession registration, workDirFor resolution).
type noopBackendManager struct {
	createdWorkDir string
}

func (m *noopBackendManager) Init(_ context.Context, _ func() ([]string, error)) error { return nil }
func (m *noopBackendManager) CreateBackend(_ context.Context, inv agent.BackendInvocation) (agent.SessionBackend, error) {
	m.createdWorkDir = inv.WorkDir
	return &noopBackend{}, nil
}
func (m *noopBackendManager) Shutdown() {}

type noopBackend struct{}

func (b *noopBackend) Open(_ context.Context) error { return nil }
func (b *noopBackend) OpenAndSend(_ context.Context, _ agent.SendMessageOpts) error {
	return nil
}
func (b *noopBackend) Send(_ context.Context, _ agent.SendMessageOpts) error {
	return nil
}
func (b *noopBackend) Abort(_ context.Context) error { return nil }
func (b *noopBackend) Stop() error                   { return nil }
func (b *noopBackend) Events() <-chan agent.Event {
	// Return a closed channel so callers that range over events
	// terminate immediately instead of blocking forever on nil.
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (b *noopBackend) Status() agent.SessionStatus { return agent.StatusIdle }
func (b *noopBackend) SessionID() string           { return "stub" }
func (b *noopBackend) Messages(_ context.Context) ([]agent.MessageData, error) {
	return nil, nil
}
func (b *noopBackend) Revert(_ context.Context, _ string) error { return nil }
func (b *noopBackend) Fork(_ context.Context, _ string) (agent.ForkResult, error) {
	return agent.ForkResult{}, nil
}
func (b *noopBackend) RespondPermission(_ context.Context, _ string, _ bool) error { return nil }

func newTestService(t *testing.T) *host.Service {
	t.Helper()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		ClonesDir: t.TempDir(),
	})
	t.Cleanup(svc.Shutdown)
	return svc
}

// initGitRepo creates a real git repo with an "origin" remote so the
// host can validate a GitRef.Local path and run git ops against it.
func initGitRepo(t *testing.T, remote string) string {
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
	run("git", "remote", "add", "origin", remote)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "initial")
	return dir
}
