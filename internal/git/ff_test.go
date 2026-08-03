package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supaclank/clank/internal/git"
)

// TestFetchBundleObjects_AndIsAncestor pins the fast-forward primitives
// `clank pull` relies on: load a remote bundle's objects into a repo
// without touching its branch/worktree, then check ancestry — true when
// local HEAD is behind the remote (fast-forwardable), false when local
// has diverged.
func TestFetchBundleObjects_AndIsAncestor(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repoA := t.TempDir()
	ffGit(t, repoA, "init", "-q")
	ffGit(t, repoA, "config", "user.email", "t@e.com")
	ffGit(t, repoA, "config", "user.name", "t")
	ffWrite(t, filepath.Join(repoA, "f.txt"), "v1")
	ffGit(t, repoA, "add", ".")
	ffGit(t, repoA, "commit", "-qm", "c1")
	commit1 := ffRev(t, repoA, "HEAD")

	// repoB clones A at commit1 (HEAD = commit1), before A advances.
	repoB := t.TempDir()
	ffGit(t, "", "clone", "-q", repoA, repoB)

	// A advances to commit2 (not yet in repoB).
	ffWrite(t, filepath.Join(repoA, "f.txt"), "v2")
	ffGit(t, repoA, "add", ".")
	ffGit(t, repoA, "commit", "-qm", "c2")
	commit2 := ffRev(t, repoA, "HEAD")

	bundle := filepath.Join(t.TempDir(), "a.bundle")
	ffGit(t, repoA, "bundle", "create", bundle, "--all")

	if ffHas(t, repoB, commit2) {
		t.Fatalf("precondition: repoB unexpectedly already has %s", commit2)
	}
	if err := git.FetchBundleObjects(repoB, bundle); err != nil {
		t.Fatalf("FetchBundleObjects: %v", err)
	}
	if !ffHas(t, repoB, commit2) {
		t.Fatalf("after FetchBundleObjects, repoB still lacks %s", commit2)
	}

	// Fast-forward: repoB HEAD (commit1) is an ancestor of commit2.
	ff, err := git.IsAncestor(repoB, commit1, commit2)
	if err != nil {
		t.Fatalf("IsAncestor(ff): %v", err)
	}
	if !ff {
		t.Error("commit1 should be an ancestor of commit2 (fast-forwardable)")
	}

	// Diverged: repoB makes its own commit on top of commit1.
	ffGit(t, repoB, "reset", "--hard", commit1)
	ffWrite(t, filepath.Join(repoB, "f.txt"), "local-divergent")
	ffGit(t, repoB, "add", ".")
	ffGit(t, repoB, "commit", "-qm", "local")
	commitX := ffRev(t, repoB, "HEAD")

	diverged, err := git.IsAncestor(repoB, commitX, commit2)
	if err != nil {
		t.Fatalf("IsAncestor(diverged): %v", err)
	}
	if diverged {
		t.Error("a local divergent commit must NOT be an ancestor of remote commit2")
	}
}

func ffGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func ffRev(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func ffHas(t *testing.T, dir, sha string) bool {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "-e", sha+"^{commit}")
	cmd.Dir = dir
	return cmd.Run() == nil
}

func ffWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
