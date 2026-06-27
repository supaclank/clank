package git_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acksell/clank/internal/git"
)

// cloneAtC1 builds repoA with a single commit (f.txt=base, plus any extra
// files) and a repoB cloned from it, returning both paths and c1's SHA.
// repoB has an `origin` remote pointing at repoA, so tests can `fetch
// origin` to populate FETCH_HEAD with whatever repoA advances to.
func cloneAtC1(t *testing.T, extra map[string]string) (repoA, repoB, c1 string) {
	t.Helper()
	repoA = t.TempDir()
	ffGit(t, repoA, "init", "-q")
	ffGit(t, repoA, "config", "user.email", "t@e.com")
	ffGit(t, repoA, "config", "user.name", "t")
	ffWrite(t, filepath.Join(repoA, "f.txt"), "base")
	for name, content := range extra {
		ffWrite(t, filepath.Join(repoA, name), content)
	}
	ffGit(t, repoA, "add", ".")
	ffGit(t, repoA, "commit", "-qm", "c1")
	c1 = ffRev(t, repoA, "HEAD")

	repoB = t.TempDir()
	ffGit(t, "", "clone", "-q", repoA, repoB)
	return repoA, repoB, c1
}

func TestMergeFF(t *testing.T) {
	t.Parallel()
	repoA, repoB, c1 := cloneAtC1(t, nil)

	// repoA advances; repoB fetches it into FETCH_HEAD.
	ffWrite(t, filepath.Join(repoA, "f.txt"), "v2")
	ffGit(t, repoA, "add", ".")
	ffGit(t, repoA, "commit", "-qm", "c2")
	c2 := ffRev(t, repoA, "HEAD")
	ffGit(t, repoB, "fetch", "-q", "origin")

	if err := git.MergeFF(repoB, "FETCH_HEAD"); err != nil {
		t.Fatalf("MergeFF (fast-forwardable): %v", err)
	}
	if got := ffRev(t, repoB, "HEAD"); got != c2 {
		t.Fatalf("after MergeFF HEAD = %s, want c2 %s", got, c2)
	}

	// Diverge repoB, then a fast-forward must be refused.
	ffGit(t, repoB, "reset", "--hard", c1)
	ffWrite(t, filepath.Join(repoB, "f.txt"), "local")
	ffGit(t, repoB, "add", ".")
	ffGit(t, repoB, "commit", "-qm", "local")
	ffGit(t, repoB, "fetch", "-q", "origin")
	if err := git.MergeFF(repoB, "FETCH_HEAD"); err == nil {
		t.Fatal("MergeFF on a diverged branch should error, got nil")
	}
}

func TestMergeCleanCreatesMergeCommit(t *testing.T) {
	t.Parallel()
	// a.txt and b.txt let each side touch a different file (auto-mergeable).
	repoA, repoB, _ := cloneAtC1(t, map[string]string{"a.txt": "a0", "b.txt": "b0"})

	ffWrite(t, filepath.Join(repoA, "a.txt"), "a-remote")
	ffGit(t, repoA, "add", ".")
	ffGit(t, repoA, "commit", "-qm", "c2-remote")

	ffWrite(t, filepath.Join(repoB, "b.txt"), "b-local")
	ffGit(t, repoB, "add", ".")
	ffGit(t, repoB, "commit", "-qm", "local")
	ffGit(t, repoB, "fetch", "-q", "origin")

	if err := git.Merge(repoB, "FETCH_HEAD", "Merge remote"); err != nil {
		t.Fatalf("Merge (clean): %v", err)
	}
	if git.IsMerging(repoB) {
		t.Fatal("repo still mid-merge after a clean Merge")
	}
	// A real merge commit (diverged histories) has a second parent.
	ffGit(t, repoB, "rev-parse", "HEAD^2")
	if got := readFile(t, filepath.Join(repoB, "a.txt")); got != "a-remote" {
		t.Errorf("a.txt = %q, want a-remote (remote change not merged in)", got)
	}
	if got := readFile(t, filepath.Join(repoB, "b.txt")); got != "b-local" {
		t.Errorf("b.txt = %q, want b-local (local change lost)", got)
	}
}

func TestMergeConflictReturnsTypedError(t *testing.T) {
	t.Parallel()
	repoA, repoB, _ := cloneAtC1(t, nil)

	// Both sides edit f.txt → conflict.
	ffWrite(t, filepath.Join(repoA, "f.txt"), "remote-change")
	ffGit(t, repoA, "add", ".")
	ffGit(t, repoA, "commit", "-qm", "c2-remote")

	ffWrite(t, filepath.Join(repoB, "f.txt"), "local-change")
	ffGit(t, repoB, "add", ".")
	ffGit(t, repoB, "commit", "-qm", "local")
	ffGit(t, repoB, "fetch", "-q", "origin")

	err := git.Merge(repoB, "FETCH_HEAD", "Merge remote")
	if !errors.Is(err, git.ErrMergeConflict) {
		t.Fatalf("Merge conflict err = %v, want ErrMergeConflict", err)
	}
	if !git.IsMerging(repoB) {
		t.Fatal("repo should be mid-merge after a conflicting Merge")
	}
	files, err := git.ConflictedFiles(repoB)
	if err != nil {
		t.Fatalf("ConflictedFiles: %v", err)
	}
	if !slices.Contains(files, "f.txt") {
		t.Errorf("ConflictedFiles = %v, want it to contain f.txt", files)
	}
	if err := git.AbortMerge(repoB); err != nil {
		t.Fatalf("AbortMerge: %v", err)
	}
}

func TestResetHardWithBackupRef(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	ffGit(t, repo, "init", "-q")
	ffGit(t, repo, "config", "user.email", "t@e.com")
	ffGit(t, repo, "config", "user.name", "t")
	ffWrite(t, filepath.Join(repo, "f.txt"), "v1")
	ffGit(t, repo, "add", ".")
	ffGit(t, repo, "commit", "-qm", "c1")
	c1 := ffRev(t, repo, "HEAD")
	ffWrite(t, filepath.Join(repo, "f.txt"), "v2")
	ffGit(t, repo, "add", ".")
	ffGit(t, repo, "commit", "-qm", "c2")
	c2 := ffRev(t, repo, "HEAD")

	const backup = "refs/clank/backup/test"
	if err := git.BackupRef(repo, backup, "HEAD"); err != nil {
		t.Fatalf("BackupRef: %v", err)
	}
	if err := git.ResetHard(repo, c1); err != nil {
		t.Fatalf("ResetHard: %v", err)
	}
	if got := ffRev(t, repo, "HEAD"); got != c1 {
		t.Errorf("after ResetHard HEAD = %s, want c1 %s", got, c1)
	}
	// The discarded commit survives via the backup ref.
	if got := ffRev(t, repo, backup); got != c2 {
		t.Errorf("backup ref = %s, want c2 %s", got, c2)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
