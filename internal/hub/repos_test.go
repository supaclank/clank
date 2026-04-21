package hub_test

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostclient "github.com/acksell/clank/internal/host/client"
	hostmux "github.com/acksell/clank/internal/host/mux"
	"github.com/acksell/clank/internal/hub"
)

// noopHostBackendManager is a tiny BackendManager fixture local to this
// test file (not a mock of the SUT) so we can construct a real
// host.Service. The agents_models_test fixtures live in package hub
// internals so we can't reuse them directly.
type noopHostBackendManager struct{}

func (m *noopHostBackendManager) Init(_ context.Context, _ func() ([]string, error)) error {
	return nil
}
func (m *noopHostBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	return nil, nil
}
func (m *noopHostBackendManager) Shutdown() {}

func initRepoForHub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "initial")
	return dir
}

// TestHubWorktreesEndToEnd exercises the §7 path-free wire shape:
// hubclient.Host(h).{ListBranches,ResolveWorktree,RemoveWorktree}
// → hub mux → *hostclient.HTTP → httptest server → hostmux →
// host.Service.workDirFor → real git. The host registry is gone — refs
// resolve via local path or deterministic clone path with no AddRepo call.
func TestHubWorktreesEndToEnd(t *testing.T) {
	t.Parallel()

	dir := initRepoForHub(t)

	hostSvc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopHostBackendManager{},
		},
		ClonesDir: t.TempDir(),
	})
	t.Cleanup(hostSvc.Shutdown)

	hostHTTP := httptest.NewServer(hostmux.New(hostSvc, nil).Handler())
	t.Cleanup(hostHTTP.Close)
	hostC := hostclient.NewHTTP(hostHTTP.URL, nil)
	t.Cleanup(func() { _ = hostC.Close() })

	s := hub.New()
	s.SetHostClient(hostC)

	client, _, cleanup := startHubOnSocket(t, s)
	defer cleanup()

	ctx := context.Background()
	ref := agent.GitRef{LocalPath: dir}

	// ListBranches runs against real git.
	branches, err := client.Host(host.HostLocal).ListBranches(ctx, ref)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(branches) == 0 || branches[0].Name != "main" {
		t.Fatalf("branches = %+v", branches)
	}

	// ResolveWorktree creates a new branch + worktree.
	wt, err := client.Host(host.HostLocal).ResolveWorktree(ctx, ref, "feat/x")
	if err != nil {
		t.Fatalf("ResolveWorktree: %v", err)
	}
	if wt.WorktreeDir == "" || wt.Branch != "feat/x" {
		t.Fatalf("wt = %+v", wt)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wt.WorktreeDir) })

	// RemoveWorktree cleans up.
	if err := client.Host(host.HostLocal).RemoveWorktree(ctx, ref, "feat/x", true); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
}

// TestHubWorktreesUnknownHost verifies the 404 path when hostname is
// not registered in the catalog.
func TestHubWorktreesUnknownHost(t *testing.T) {
	t.Parallel()

	s := hub.New()
	// no host registered

	client, _, cleanup := startHubOnSocket(t, s)
	defer cleanup()

	ref := agent.GitRef{LocalPath: "/does/not/matter"}
	if _, err := client.Host("ghost").ListBranches(context.Background(), ref); err == nil {
		t.Fatal("expected error for unknown host")
	}
}
