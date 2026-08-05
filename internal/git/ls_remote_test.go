package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestEnvWithTerminalPromptDisabledOverridesInheritedValue(t *testing.T) {
	// t.Setenv mutates the process environment; can't run alongside t.Parallel().
	t.Setenv("GIT_TERMINAL_PROMPT", "1")

	env := envWithTerminalPromptDisabled()

	var matches []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") {
			matches = append(matches, kv)
		}
	}
	if want := []string{"GIT_TERMINAL_PROMPT=0"}; !slices.Equal(matches, want) {
		t.Errorf("GIT_TERMINAL_PROMPT entries = %v, want %v", matches, want)
	}
}
