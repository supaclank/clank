package host_test

// Regression coverage for Options.WorkRoot: a laptop host must place
// worktrees under the configured root (the local provisioner passes
// <config dir>/work) instead of littering $HOME/work — the sprite
// default that leaked onto laptops before the option existed.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// TestCreateRepoWorktree_HonorsWorkRootOption drives the phone's
// repos → new-branch flow (fork off main) against a Service built
// with Options.WorkRoot and asserts the worktree lands under it.
func TestCreateRepoWorktree_HonorsWorkRootOption(t *testing.T) {
	t.Parallel()
	workRoot := filepath.Join(t.TempDir(), "clank-work")

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		WorkRoot: workRoot,
	})
	t.Cleanup(svc.Shutdown)

	checkout := initGitRepo(t, "https://github.com/acme/app.git")
	slug := host.LocalRepoSlug(checkout)

	result, err := svc.CreateRepoWorktree(context.Background(), slug, host.RepoWorktreeRequest{
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateRepoWorktree: %v", err)
	}
	if !result.Created {
		t.Fatal("CreateRepoWorktree: created = false, want true")
	}
	if got := filepath.Dir(result.WorktreeDir); got != workRoot {
		t.Errorf("worktree parent = %q, want the configured work root %q", got, workRoot)
	}
	if _, err := ulid.ParseStrict(filepath.Base(result.WorktreeDir)); err != nil {
		t.Errorf("worktree dir name %q is not a ULID: %v", filepath.Base(result.WorktreeDir), err)
	}
	if fi, err := os.Stat(result.WorktreeDir); err != nil || !fi.IsDir() {
		t.Errorf("worktree dir %q: stat err=%v", result.WorktreeDir, err)
	}
}
