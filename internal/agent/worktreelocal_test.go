package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestReadLocalWorktreeID_FreshRepo: a brand-new git repo has no cached
// id; Read returns "" without error.
func TestReadLocalWorktreeID_FreshRepo(t *testing.T) {
	// serial: uses t.Setenv (CLANK_WORKTREE_ID is a process global).
	repo := initGitRepo(t)
	t.Setenv(EnvWorktreeID, "")

	got, err := ReadLocalWorktreeID(repo)
	if err != nil {
		t.Fatalf("ReadLocalWorktreeID: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestWriteAndRead_RoundTrip: Write puts the id inside .git/clank/ and
// Read returns it back.
func TestWriteAndRead_RoundTrip(t *testing.T) {
	// serial: uses t.Setenv (CLANK_WORKTREE_ID is a process global).
	repo := initGitRepo(t)
	t.Setenv(EnvWorktreeID, "")

	const wantID = "01HXX0000000000000000ABCDE"
	if err := WriteLocalWorktreeID(repo, wantID); err != nil {
		t.Fatalf("WriteLocalWorktreeID: %v", err)
	}

	// Lands under .git/clank/, not <repo>/.clank/.
	if _, err := os.Stat(filepath.Join(repo, ".git", "clank", "worktree-id")); err != nil {
		t.Errorf(".git/clank/worktree-id not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".clank")); !os.IsNotExist(err) {
		t.Errorf("legacy <repo>/.clank/ should not exist; stat err = %v", err)
	}

	got, err := ReadLocalWorktreeID(repo)
	if err != nil {
		t.Fatalf("ReadLocalWorktreeID: %v", err)
	}
	if got != wantID {
		t.Errorf("got %q, want %q", got, wantID)
	}
}

// TestEnvWorktreeIDOverridesCache: when CLANK_WORKTREE_ID is set, it
// wins over the on-disk cache.
func TestEnvWorktreeIDOverridesCache(t *testing.T) {
	// serial: uses t.Setenv (CLANK_WORKTREE_ID is a process global).
	repo := initGitRepo(t)
	if err := WriteLocalWorktreeID(repo, "from-disk"); err != nil {
		t.Fatalf("WriteLocalWorktreeID: %v", err)
	}
	t.Setenv(EnvWorktreeID, "from-env")

	got, err := ReadLocalWorktreeID(repo)
	if err != nil {
		t.Fatalf("ReadLocalWorktreeID: %v", err)
	}
	if got != "from-env" {
		t.Errorf("got %q, want %q (env should beat disk)", got, "from-env")
	}
}

// TestReadLocalWorktreeID_NotAGitRepo: a non-git directory returns ""
// without error so callers like `clank status` don't blow up outside a
// repo.
func TestReadLocalWorktreeID_NotAGitRepo(t *testing.T) {
	// serial: uses t.Setenv (CLANK_WORKTREE_ID is a process global).
	dir := t.TempDir()
	t.Setenv(EnvWorktreeID, "")

	got, err := ReadLocalWorktreeID(dir)
	if err != nil {
		t.Fatalf("ReadLocalWorktreeID: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty in non-git dir", got)
	}
}

// TestLinkedWorktreeGetsItsOwnID is the headline property: a
// `git worktree add` sibling resolves to a different $gitDir, so it
// gets its own cached id rather than inheriting the main worktree's.
func TestLinkedWorktreeGetsItsOwnID(t *testing.T) {
	// serial: uses t.Setenv (CLANK_WORKTREE_ID is a process global).
	main := initGitRepo(t)
	t.Setenv(EnvWorktreeID, "")

	// A commit + branch is required for `git worktree add` to succeed.
	runGit(t, main, "commit", "--allow-empty", "-m", "init")
	runGit(t, main, "branch", "feature")

	// linkedDir is created BY git as a sibling — must not pre-exist.
	linkedDir := filepath.Join(t.TempDir(), "linked")
	runGit(t, main, "worktree", "add", linkedDir, "feature")

	if err := WriteLocalWorktreeID(main, "main-id"); err != nil {
		t.Fatalf("write main: %v", err)
	}
	if err := WriteLocalWorktreeID(linkedDir, "linked-id"); err != nil {
		t.Fatalf("write linked: %v", err)
	}

	gotMain, err := ReadLocalWorktreeID(main)
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	gotLinked, err := ReadLocalWorktreeID(linkedDir)
	if err != nil {
		t.Fatalf("read linked: %v", err)
	}
	if gotMain != "main-id" {
		t.Errorf("main id = %q, want main-id", gotMain)
	}
	if gotLinked != "linked-id" {
		t.Errorf("linked id = %q, want linked-id", gotLinked)
	}
}

// --- helpers --------------------------------------------------------

// initGitRepo creates a fresh git repo at t.TempDir() with the
// minimal config needed for `commit` to succeed. Returns the absolute
// repo path.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "--initial-branch=main", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		runGit(t, dir, args...)
	}
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
}
