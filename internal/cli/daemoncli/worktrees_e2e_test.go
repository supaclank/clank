package daemoncli

// Worktree wire roundtrip. Ported from internal/hub/repos_test.go
// (deleted in PR 3 phase 3c). Pins that the path-free wire from
// daemonclient.HostClient → gateway → host → real git keeps working
// for ListBranches/ResolveWorktree/RemoveWorktree.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// initWorktreeRepo creates a real git repo with a committed README.
// The init is on `main` so ListBranches has a default branch to find,
// and resolveWorktree has somewhere to fork off from.
func initWorktreeRepo(t *testing.T) string {
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

// TestWorktree_ListResolveRemoveRoundTrip exercises the §7.3 path-free
// wire end-to-end: list branches, resolve a new worktree off a branch
// name, then remove it. All three calls go through /hosts/{name}/...
// which the gateway strips before forwarding (regression coverage for
// the prefix-strip bug fixed in PR 3 phase 7).
func TestWorktree_ListResolveRemoveRoundTrip(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	dir := initWorktreeRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ref := agent.GitRef{LocalPath: dir}
	branches, err := td.Client.Host(host.HostLocal).ListBranches(ctx, ref)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	var sawMain bool
	for _, b := range branches {
		if b.Name == "main" {
			sawMain = true
			break
		}
	}
	if !sawMain {
		t.Errorf("ListBranches missing main; got %+v", branches)
	}

	wt, err := td.Client.Host(host.HostLocal).ResolveWorktree(ctx, ref, "feat/test")
	if err != nil {
		t.Fatalf("ResolveWorktree: %v", err)
	}
	if wt.Branch != "feat/test" {
		t.Errorf("Branch = %q, want feat/test", wt.Branch)
	}
	if wt.WorktreeDir == "" {
		t.Error("WorktreeDir empty")
	}
	t.Cleanup(func() { _ = os.RemoveAll(wt.WorktreeDir) })

	if err := td.Client.Host(host.HostLocal).RemoveWorktree(ctx, ref, "feat/test", true); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
}

// TestWorktree_RejectsDefaultBranch pins the host-side guard against
// an accidental "resolve worktree for main" when the original is
// checked out on a different branch. Creating a worktree at main
// there would lock the original out of `git checkout main`. The host
// returns ErrReservedBranch; the gateway forwards verbatim.
//
// The setup mirrors the unit test in resolve_worktree_test.go: the
// original repo must NOT be on main when we make the call, otherwise
// the host correctly resolves to the existing main checkout (not an
// error).
func TestWorktree_RejectsDefaultBranch(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	dir := initWorktreeRepo(t)

	// Move the original off main.
	cmd := exec.Command("git", "checkout", "-b", "feature")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b feature: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := td.Client.Host(host.HostLocal).ResolveWorktree(ctx, agent.GitRef{LocalPath: dir}, "main")
	if err == nil {
		t.Fatal("expected error for default-branch worktree, got nil")
	}
}
