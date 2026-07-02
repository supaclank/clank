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

// TestCreateWorktree_FetchesMissingBaseBranch reproduces the "New branch →
// HTTP 502" bug: mobile forks from a repo's *remote* branch (the picker lists
// GitHub branches), but the base worktree is a shallow single-branch clone
// (git clone --depth 1 -b <branch>) whose local repo only holds that one
// branch. Forking from any other branch used to 404 ("base branch does not
// exist") — which the gateway then masked as a 502. The host now fetches the
// branch from origin before giving up, so the fork succeeds.
func TestCreateWorktree_FetchesMissingBaseBranch(t *testing.T) {
	// Not parallel: CreateWorktree resolves ~/.clank/worktrees via
	// os.UserHomeDir(), so t.Setenv(HOME) below can't share a process
	// with other parallel tests.
	t.Setenv("HOME", t.TempDir())

	source := newSourceRepoWithBranches(t)
	clone := shallowCloneSingleBranch(t, source, "feature")

	// Precondition: main is on the remote but NOT local (single-branch clone).
	if branchExistsLocally(t, clone, "main") {
		t.Fatal("precondition failed: main should not exist locally in a single-branch clone")
	}

	svc := newTestService(t)
	ctx := context.Background()
	ref := agent.GitRef{LocalPath: clone}
	res, err := svc.CreateWorktree(ctx, ref, "main")
	if err != nil {
		t.Fatalf("CreateWorktree off missing-local base branch main: %v", err)
	}
	if res.WorktreeID == "" || res.WorktreeDir == "" {
		t.Fatalf("empty result: %+v", res)
	}
	// The forked worktree lands under ~/.clank/worktrees/<project>/<petname>;
	// remove it (and the now-empty project parent) so the test doesn't leak
	// into the real home dir.
	t.Cleanup(func() {
		_ = svc.RemoveWorktree(ctx, ref, res.Branch, true)
		if res.WorktreeDir != "" {
			_ = os.RemoveAll(filepath.Dir(res.WorktreeDir))
		}
	})

	// The new worktree must sit on top of main's commit, not feature's.
	head := strings.TrimSpace(gitOut(t, res.WorktreeDir, "log", "-1", "--pretty=%s"))
	if head != "on main" {
		t.Fatalf("new worktree HEAD subject = %q, want the main-branch commit %q", head, "on main")
	}
	// And main should now be materialized locally so future forks are cheap.
	if !branchExistsLocally(t, clone, "main") {
		t.Fatal("expected main to be fetched into the local repo after the fork")
	}
}

// TestCreateWorktree_BranchAbsentOnRemote confirms a genuinely missing branch
// still surfaces as ErrNotFound (not a wrapped transport error), so the caller
// keeps returning a 404 rather than a 5xx.
func TestCreateWorktree_BranchAbsentOnRemote(t *testing.T) {
	// Not parallel: matches TestCreateWorktree_FetchesMissingBaseBranch's
	// HOME isolation so a future change to the error path can't silently
	// start writing under the real ~/.clank/worktrees.
	t.Setenv("HOME", t.TempDir())

	source := newSourceRepoWithBranches(t)
	clone := shallowCloneSingleBranch(t, source, "feature")

	svc := newTestService(t)
	_, err := svc.CreateWorktree(context.Background(), agent.GitRef{LocalPath: clone}, "does-not-exist")
	if !errors.Is(err, host.ErrNotFound) {
		t.Fatalf("CreateWorktree off a nonexistent branch: got %v, want ErrNotFound", err)
	}
}

// newSourceRepoWithBranches builds a normal repo with two branches (main and
// feature), used as the clone origin. Returns a file:// URL so downstream
// clones honour --single-branch/--depth instead of hardlinking every ref.
func newSourceRepoWithBranches(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRunFetch(t, dir, "init", "-b", "main")
	gitRunFetch(t, dir, "config", "user.email", "t@t")
	gitRunFetch(t, dir, "config", "user.name", "T")
	writeFileT(t, filepath.Join(dir, "README"), "x\n")
	gitRunFetch(t, dir, "add", ".")
	gitRunFetch(t, dir, "commit", "-m", "on main")
	gitRunFetch(t, dir, "checkout", "-b", "feature")
	writeFileT(t, filepath.Join(dir, "feature.txt"), "f\n")
	gitRunFetch(t, dir, "add", ".")
	gitRunFetch(t, dir, "commit", "-m", "on feature")
	// Leave HEAD on feature so the shallow clone checks that branch out.
	return "file://" + dir
}

func shallowCloneSingleBranch(t *testing.T, sourceURL, branch string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "clone")
	gitRunFetch(t, "", "clone", "--depth", "1", "--single-branch", "--branch", branch, sourceURL, dst)
	return dst
}

func branchExistsLocally(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch)
	cmd.Dir = repo
	return cmd.Run() == nil
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// gitRunFetch is a local git runner named to avoid colliding with the package's
// other gitRun helpers.
func gitRunFetch(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
