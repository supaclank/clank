package worktreescope

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

func TestWorktreesForRepo_EnumeratesAndMapsIDs(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)

	linked := filepath.Join(t.TempDir(), "feature")
	runGit(t, repo, "worktree", "add", "-q", "-b", "feature", linked)

	// Track the linked worktree; leave main untracked.
	if err := agent.WriteLocalWorktreeID(linked, "wt_linked_123"); err != nil {
		t.Fatal(err)
	}

	scopes, err := WorktreesForRepo(repo, DefaultRecencyWindow)
	if err != nil {
		t.Fatalf("WorktreesForRepo: %v", err)
	}
	byPath := map[string]Scope{}
	for _, s := range scopes {
		byPath[normalize(t, s.Path)] = s
	}

	main, ok := byPath[normalize(t, repo)]
	if !ok {
		t.Fatalf("main worktree missing; got %v", keys(byPath))
	}
	if main.WorktreeID != "" {
		t.Errorf("main WorktreeID = %q, want empty (untracked)", main.WorktreeID)
	}
	if !main.IsRecentlyActive {
		t.Error("freshly-created main worktree should be recently active")
	}

	feat, ok := byPath[normalize(t, linked)]
	if !ok {
		t.Fatalf("linked worktree missing; got %v", keys(byPath))
	}
	if feat.WorktreeID != "wt_linked_123" {
		t.Errorf("linked WorktreeID = %q, want wt_linked_123", feat.WorktreeID)
	}
	if feat.Branch != "feature" {
		t.Errorf("linked Branch = %q, want feature", feat.Branch)
	}
}

func TestWorktreesForRepo_RecencyBoundary(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := initRepo(t)

	gd, err := agent.GitDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(gd, "index"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(repo, old, old); err != nil {
		t.Fatal(err)
	}

	scopes, err := WorktreesForRepo(repo, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 {
		t.Fatalf("want 1 worktree, got %d", len(scopes))
	}
	if scopes[0].IsRecentlyActive {
		t.Error("a worktree idle for 72h should not be recently active (window 48h)")
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "t@example.com")
	runGit(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "init")
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func normalize(t *testing.T, p string) string {
	t.Helper()
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func keys(m map[string]Scope) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
