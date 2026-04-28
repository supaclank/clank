package host_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// TestListBranches_CachedWithinTTL is a regression test for a CPU spike
// in clank-host: the TUI inbox polls ListBranches every 3s on every
// open session worktree. Without caching, every poll fans out to ~4
// git subprocesses per active worktree (DiffStat = 3, CommitsAhead =
// 1) — including a working-tree-stat'ing `git diff --numstat HEAD`.
// With 2 active sessions on a busy repo this pegged clank-host's CPU.
//
// The cache's correctness is observable: a second call within the TTL
// must return the same data the first call did, even if the worktree
// has been mutated under the host's feet. After advancing the clock
// past the TTL, the host must observe the new state.
func TestListBranches_CachedWithinTTL(t *testing.T) {
	t.Parallel()

	repo := initGitRepo(t, "git@example.com:acme/widget.git")

	// Add a feature worktree with one committed change so the per-worktree
	// branch (DiffStat + CommitsAhead) computation has work to do.
	addCommittedFileOnBranch(t, repo, "feature", "a.txt", "hello\n")

	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		ClonesDir:      t.TempDir(),
		BranchCacheTTL: 5 * time.Second,
		Now:            now,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	ref := agent.GitRef{LocalPath: repo}

	first, err := svc.ListBranches(ctx, ref)
	if err != nil {
		t.Fatalf("first ListBranches: %v", err)
	}
	feat1 := findBranch(t, first, "feature")
	if feat1.LinesAdded == 0 {
		t.Fatalf("expected non-zero LinesAdded for feature branch, got %+v", feat1)
	}

	// Mutate the feature worktree out-of-band so a *recomputed* result
	// would differ. If the cache is doing its job, the next call within
	// the TTL must return the original snapshot regardless. Stage the
	// file so it shows up in `git diff --numstat HEAD` (untracked files
	// are invisible to that command).
	feat := worktreeDirFor(t, repo, "feature")
	if err := os.WriteFile(filepath.Join(feat, "huge.txt"), []byte(strings.Repeat("x\n", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, feat, "add", "huge.txt")

	// Tick within TTL.
	clock.Add(int64(2 * time.Second))
	cached, err := svc.ListBranches(ctx, ref)
	if err != nil {
		t.Fatalf("cached ListBranches: %v", err)
	}
	feat2 := findBranch(t, cached, "feature")
	if feat2.LinesAdded != feat1.LinesAdded || feat2.LinesRemoved != feat1.LinesRemoved {
		t.Fatalf("cache miss within TTL: first=%+v second=%+v (worktree mutation should not be observable until TTL expires)", feat1, feat2)
	}

	// Past TTL → recomputation must observe the new working-tree state.
	clock.Add(int64(10 * time.Second))
	fresh, err := svc.ListBranches(ctx, ref)
	if err != nil {
		t.Fatalf("post-TTL ListBranches: %v", err)
	}
	feat3 := findBranch(t, fresh, "feature")
	if feat3.LinesAdded <= feat1.LinesAdded {
		t.Fatalf("expected post-TTL recomputation to pick up new lines: before=%d after=%d", feat1.LinesAdded, feat3.LinesAdded)
	}
}

// TestListBranches_InvalidatedOnResolveWorktree is a regression test for
// stale-cache UX: creating a new worktree must be reflected by the very
// next ListBranches call, regardless of TTL. Without invalidation the
// sidebar would show the old branch list for up to TTL seconds after
// the user creates a branch.
func TestListBranches_InvalidatedOnResolveWorktree(t *testing.T) {
	t.Parallel()

	repo := initGitRepo(t, "git@example.com:acme/widget.git")

	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		ClonesDir:      t.TempDir(),
		BranchCacheTTL: time.Hour, // long TTL to prove invalidation, not expiry, fixes it
		Now:            now,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	ref := agent.GitRef{LocalPath: repo}

	first, err := svc.ListBranches(ctx, ref)
	if err != nil {
		t.Fatalf("first ListBranches: %v", err)
	}
	if findBranchOrNil(first, "feature-invalidate") != nil {
		t.Fatalf("did not expect 'feature-invalidate' branch yet: %+v", first)
	}

	if _, err := svc.ResolveWorktree(ctx, ref, "feature-invalidate"); err != nil {
		t.Fatalf("ResolveWorktree: %v", err)
	}
	t.Cleanup(func() { _ = svc.RemoveWorktree(ctx, ref, "feature-invalidate", true) })

	after, err := svc.ListBranches(ctx, ref)
	if err != nil {
		t.Fatalf("post-resolve ListBranches: %v", err)
	}
	if findBranchOrNil(after, "feature-invalidate") == nil {
		t.Fatalf("expected 'feature-invalidate' branch after ResolveWorktree, got: %+v (cache was not invalidated)", after)
	}
}

// --- helpers ---

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func findBranch(t *testing.T, branches []host.BranchInfo, name string) host.BranchInfo {
	t.Helper()
	if b := findBranchOrNil(branches, name); b != nil {
		return *b
	}
	t.Fatalf("branch %q not found in %+v", name, branches)
	return host.BranchInfo{}
}

func findBranchOrNil(branches []host.BranchInfo, name string) *host.BranchInfo {
	for i := range branches {
		if branches[i].Name == name {
			return &branches[i]
		}
	}
	return nil
}

func addCommittedFileOnBranch(t *testing.T, repo, branch, file, content string) {
	t.Helper()
	wtDir := filepath.Join(t.TempDir(), branch)
	run := func(dir string, args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(repo, "git", "worktree", "add", "-b", branch, wtDir)
	if err := os.WriteFile(filepath.Join(wtDir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(wtDir, "git", "add", ".")
	run(wtDir, "git", "commit", "-m", "add "+file)
}

func worktreeDirFor(t *testing.T, repo, branch string) string {
	t.Helper()
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v\n%s", err, out)
	}
	var curPath string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			curPath = strings.TrimPrefix(line, "worktree ")
		}
		if line == "branch refs/heads/"+branch {
			return curPath
		}
	}
	t.Fatalf("worktree for branch %q not found", branch)
	return ""
}
