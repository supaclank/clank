package hosttest

import (
	"os/exec"
	"strings"
	"testing"
)

// InitGitRepo creates a git repo with an "origin" remote so
// host.workDirFor accepts a LocalPath at (or inside) it.
func InitGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	run("git", "remote", "add", "origin", "git@example.com:acme/repo.git")
	run("git", "commit", "--allow-empty", "-m", "initial")
	return dir
}
