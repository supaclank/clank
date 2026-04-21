package host_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// TestResolveWorktree_RejectsDefaultBranch_Main is a regression test for
// the bug where ResolveWorktree(ctx, ref, "main") would silently create
// a worktree at ~/.clank/worktrees/<repo>/main when the original repo
// happened to be on a non-default branch. That worktree would lock the
// default branch out of the original repo, breaking
// `git checkout main` there.
//
// Setup mirrors the real failure: the original repo is checked out on a
// non-default branch (so FindWorktreeForBranch("main") returns nil), and
// then a caller asks to "resolve" main. Pre-fix this would create a
// worktree; post-fix it must return ErrReservedBranch.
func TestResolveWorktree_RejectsDefaultBranch_Main(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t, "git@example.com:acme/widget.git")
	checkoutNewBranch(t, repo, "feature")

	svc := newTestService(t)
	ctx := context.Background()
	_, err := svc.ResolveWorktree(ctx, agent.GitRef{LocalPath: repo}, "main")
	if !errors.Is(err, host.ErrReservedBranch) {
		t.Fatalf("expected ErrReservedBranch, got %v", err)
	}
	// And no worktree directory should have been created.
	wts := listWorktrees(t, repo)
	for _, w := range wts {
		if strings.HasSuffix(w, "/main") {
			t.Fatalf("worktree for main should not exist; got worktrees: %v", wts)
		}
	}
}

// TestResolveWorktree_RejectsDefaultBranch_Master proves we use the
// dynamically-detected default branch, not a hardcoded "main".
func TestResolveWorktree_RejectsDefaultBranch_Master(t *testing.T) {
	t.Parallel()
	repo := initGitRepoWithDefault(t, "master", "git@example.com:acme/widget.git")
	checkoutNewBranch(t, repo, "feature")

	svc := newTestService(t)
	ctx := context.Background()
	_, err := svc.ResolveWorktree(ctx, agent.GitRef{LocalPath: repo}, "master")
	if !errors.Is(err, host.ErrReservedBranch) {
		t.Fatalf("expected ErrReservedBranch, got %v", err)
	}
}

// TestResolveWorktree_FindsExistingDefaultBranchWorktree confirms the
// guard does NOT break the legitimate lookup case: when the original
// repo is currently on the default branch, asking for "main" must
// return that existing worktree (the original repo path) rather than
// erroring. Session bootstrap depends on this.
func TestResolveWorktree_FindsExistingDefaultBranchWorktree(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t, "git@example.com:acme/widget.git")
	// repo is on "main" by default.

	svc := newTestService(t)
	wt, err := svc.ResolveWorktree(context.Background(), agent.GitRef{LocalPath: repo}, "main")
	if err != nil {
		t.Fatalf("expected lookup to succeed, got %v", err)
	}
	if wt.WorktreeDir != repo {
		// On macOS t.TempDir() may return a path under /var that resolves
		// to /private/var; the host returns the resolved path. Compare
		// canonicalized.
		got, _ := filepath.EvalSymlinks(wt.WorktreeDir)
		want, _ := filepath.EvalSymlinks(repo)
		if got != want {
			t.Fatalf("expected worktree dir %q, got %q", repo, wt.WorktreeDir)
		}
	}
	if wt.Branch != "main" {
		t.Fatalf("expected branch %q, got %q", "main", wt.Branch)
	}
}

// TestResolveWorktree_RejectsEmptyBranch ensures the host layer
// validates input even though the TUI also checks. A direct API call
// with "" must not shell out to git with an empty branch arg.
func TestResolveWorktree_RejectsEmptyBranch(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t, "git@example.com:acme/widget.git")

	svc := newTestService(t)
	for _, name := range []string{"", "   ", "\t"} {
		_, err := svc.ResolveWorktree(context.Background(), agent.GitRef{LocalPath: repo}, name)
		if !errors.Is(err, host.ErrInvalidBranchName) {
			t.Fatalf("name=%q: expected ErrInvalidBranchName, got %v", name, err)
		}
	}
}

// TestResolveWorktree_CreatesNonDefaultBranch is the happy path: a
// regular branch name should still create a worktree as before.
func TestResolveWorktree_CreatesNonDefaultBranch(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t, "git@example.com:acme/widget.git")

	svc := newTestService(t)
	wt, err := svc.ResolveWorktree(context.Background(), agent.GitRef{LocalPath: repo}, "feat-x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wt.WorktreeDir == "" {
		t.Fatalf("expected non-empty worktree dir")
	}
	if _, err := os.Stat(wt.WorktreeDir); err != nil {
		t.Fatalf("worktree dir not created: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wt.WorktreeDir) })
}

// --- helpers ---

func checkoutNewBranch(t *testing.T, repo, branch string) {
	t.Helper()
	cmd := exec.Command("git", "checkout", "-b", branch)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b %s: %v\n%s", branch, err, out)
	}
}

func listWorktrees(t *testing.T, repo string) []string {
	t.Helper()
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v\n%s", err, out)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			paths = append(paths, strings.TrimPrefix(line, "worktree "))
		}
	}
	return paths
}

// initGitRepoWithDefault is like initGitRepo but lets the caller pick
// the default branch name (e.g. "master") so we can prove the reserved-
// branch check uses git.DefaultBranch rather than a hardcoded string.
func initGitRepoWithDefault(t *testing.T, defaultBranch, remote string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", defaultBranch)
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
