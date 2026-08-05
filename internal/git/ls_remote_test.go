package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLsRemoteDefaultBranchReadsSymrefHead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "trunk")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "initial")

	branch, err := LsRemoteDefaultBranch(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatalf("LsRemoteDefaultBranch: %v", err)
	}
	if branch != "trunk" {
		t.Errorf("branch = %q, want trunk", branch)
	}
}

func TestLsRemoteDefaultBranchFailsForUnreachableRemote(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent.git")
	if _, err := LsRemoteDefaultBranch(context.Background(), "file://"+missing); err == nil {
		t.Fatal("expected an error for a nonexistent remote")
	}
}
