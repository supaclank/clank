package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPush_LocalBareRepoRoundTrip is the happy-path test: init a
// real git repo, commit something, push to a real bare repo on disk
// (the "remote"), and verify the bare repo now has the ref.
//
// File:// URLs don't exercise the HTTP auth-header path — that's
// exercised at the host integration level (PR 3 follow-up). Here we
// only confirm Push() invokes git correctly and assembles its args.
func TestPush_LocalBareRepoRoundTrip(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "work")
	bareDir := filepath.Join(tmp, "remote.git")

	mustGit(t, "", "init", workDir)
	mustGit(t, "", "init", "--bare", bareDir)
	mustGit(t, workDir, "config", "user.email", "test@example.com")
	mustGit(t, workDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workDir, "README"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "README")
	mustGit(t, workDir, "commit", "-m", "init")
	mustGit(t, workDir, "branch", "-M", "main")
	mustGit(t, workDir, "remote", "add", "origin", bareDir)

	if err := Push(workDir, "origin", "main:refs/heads/main", PushOptions{}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Verify the bare repo received the ref.
	out := mustGit(t, bareDir, "show-ref", "refs/heads/main")
	if !strings.Contains(out, "refs/heads/main") {
		t.Fatalf("bare repo missing pushed ref:\n%s", out)
	}
}

// TestPush_ExtraHeaderNotPersistedToConfig pins the security
// invariant: the auth header lives only in process args. After a
// push that uses ExtraHeader, .git/config must NOT contain the
// header value. We use a deliberately bogus header against a local
// bare repo (which doesn't enforce HTTP auth) so the push still
// succeeds — the assertion is purely about persistence.
func TestPush_ExtraHeaderNotPersistedToConfig(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "work")
	bareDir := filepath.Join(tmp, "remote.git")
	const sentinel = "Basic-not-a-real-token-SECRET"

	mustGit(t, "", "init", workDir)
	mustGit(t, "", "init", "--bare", bareDir)
	mustGit(t, workDir, "config", "user.email", "test@example.com")
	mustGit(t, workDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workDir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "README")
	mustGit(t, workDir, "commit", "-m", "x")
	mustGit(t, workDir, "branch", "-M", "main")
	mustGit(t, workDir, "remote", "add", "origin", bareDir)

	if err := Push(workDir, "origin", "main:refs/heads/main", PushOptions{
		ExtraHeader: "Authorization: " + sentinel,
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	cfgData, err := os.ReadFile(filepath.Join(workDir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfgData), sentinel) {
		t.Errorf(".git/config leaked the auth header value!\n%s", cfgData)
	}
	if strings.Contains(string(cfgData), "extraheader") {
		t.Errorf(".git/config contains 'extraheader' — expected only process-arg use:\n%s", cfgData)
	}
}

// TestPush_RejectedNotFastForward covers the classifier path. Set
// up a divergent history on origin and try to push — git refuses
// with "non-fast-forward" and we map that to ErrPushNotFastForward.
func TestPush_RejectedNotFastForward(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "work")
	bareDir := filepath.Join(tmp, "remote.git")

	mustGit(t, "", "init", workDir)
	mustGit(t, "", "init", "--bare", bareDir)
	mustGit(t, workDir, "config", "user.email", "test@example.com")
	mustGit(t, workDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workDir, "README"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "README")
	mustGit(t, workDir, "commit", "-m", "v1")
	mustGit(t, workDir, "branch", "-M", "main")
	mustGit(t, workDir, "remote", "add", "origin", bareDir)
	// First push lands cleanly so the remote has main.
	if err := Push(workDir, "origin", "main:refs/heads/main", PushOptions{}); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// Now create a sibling working tree, commit something different,
	// and try to push it. Bare repo's main will diverge.
	otherDir := filepath.Join(tmp, "other")
	mustGit(t, "", "clone", "--branch", "main", bareDir, otherDir)
	mustGit(t, otherDir, "config", "user.email", "test@example.com")
	mustGit(t, otherDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(otherDir, "README"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, otherDir, "add", "README")
	mustGit(t, otherDir, "commit", "-m", "v2-via-other")
	if err := Push(otherDir, "origin", "main:refs/heads/main", PushOptions{}); err != nil {
		t.Fatalf("seed divergence push: %v", err)
	}

	// Original workDir's main is now behind. Add a different commit
	// on it and push — should be rejected.
	if err := os.WriteFile(filepath.Join(workDir, "README"), []byte("v2-via-work"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "README")
	mustGit(t, workDir, "commit", "-m", "v2-via-work")

	err := Push(workDir, "origin", "main:refs/heads/main", PushOptions{})
	if !errors.Is(err, ErrPushNotFastForward) {
		t.Fatalf("err = %v, want ErrPushNotFastForward", err)
	}
}

// TestPush_RepositoryNotFound covers the classifier path for
// remotes that don't exist. We point at a nonexistent bare repo and
// expect ErrPushRepoNotFound.
func TestPush_RepositoryNotFound(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workDir := filepath.Join(tmp, "work")
	missing := filepath.Join(tmp, "does-not-exist.git")

	mustGit(t, "", "init", workDir)
	mustGit(t, workDir, "config", "user.email", "test@example.com")
	mustGit(t, workDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workDir, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "README")
	mustGit(t, workDir, "commit", "-m", "hi")
	mustGit(t, workDir, "branch", "-M", "main")
	mustGit(t, workDir, "remote", "add", "origin", missing)

	err := Push(workDir, "origin", "main:refs/heads/main", PushOptions{})
	if !errors.Is(err, ErrPushRepoNotFound) {
		t.Fatalf("err = %v, want ErrPushRepoNotFound (got: %v)", err, err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %q: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}
