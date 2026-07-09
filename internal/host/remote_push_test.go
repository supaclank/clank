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
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// TestPushToRemote_RefusesPushToUnrelatedRepo is the push-flow twin of
// TestCreatePR_RefusesPushToUnrelatedRepo: the mobile "Push to remote"
// button must refuse (ErrNoCommonAncestor) when origin points at a
// repo sharing no history with the worktree, instead of creating the
// branch there and leaking code.
func TestPushToRemote_RefusesPushToUnrelatedRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000400"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	wrongBareDir := filepath.Join(homeDir, "wrong.git")

	// The user's worktree on its own history, with uncommitted work —
	// the exact state the Push button is for.
	mustGit(t, "", "init", workdir)
	mustGit(t, workdir, "config", "user.email", "test@example.com")
	mustGit(t, workdir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("our v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "our base")
	mustGit(t, workdir, "branch", "-M", "main")
	mustGit(t, workdir, "checkout", "-b", "feat-x")
	if err := os.WriteFile(filepath.Join(workdir, "SECRET"), []byte("must not leak\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The WRONG bare repo on a completely independent history.
	wrongStaging := filepath.Join(homeDir, "wrong-staging")
	mustGit(t, "", "init", wrongStaging)
	mustGit(t, wrongStaging, "config", "user.email", "other@example.com")
	mustGit(t, wrongStaging, "config", "user.name", "other")
	if err := os.WriteFile(filepath.Join(wrongStaging, "DIFFERENT"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wrongStaging, "add", "DIFFERENT")
	mustGit(t, wrongStaging, "commit", "-m", "their unrelated base")
	mustGit(t, wrongStaging, "branch", "-M", "main")
	mustGit(t, "", "init", "--bare", wrongBareDir)
	mustGit(t, wrongStaging, "remote", "add", "wrongorigin", wrongBareDir)
	mustGit(t, wrongStaging, "push", "wrongorigin", "main")

	// Point origin at the wrong repo.
	mustGit(t, workdir, "remote", "add", "origin", "https://github.com/wrong/repo.git")
	mustGit(t, workdir, "config", "url."+wrongBareDir+".insteadOf", "https://github.com/wrong/repo.git")

	store := githubpkg.NewStore(homeDir)
	if err := store.Write(githubpkg.Credentials{
		AccessToken: "gho_test",
		GitHubLogin: "axelengstrom",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	refsBefore := mustGit(t, wrongBareDir, "show-ref")

	prev := host.SetWorkRootForTest(filepath.Join(homeDir, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	_, err := svc.PushToRemote(context.Background(), worktreeID)
	if !errors.Is(err, host.ErrNoCommonAncestor) {
		t.Fatalf("err = %v, want ErrNoCommonAncestor", err)
	}

	// Nothing reached the wrong repo.
	refsAfter := mustGit(t, wrongBareDir, "show-ref")
	if refsBefore != refsAfter {
		t.Errorf("wrong bare repo's refs changed after refused push!\nbefore:\n%s\nafter:\n%s",
			refsBefore, refsAfter)
	}
	if strings.Contains(refsAfter, "feat-x") {
		t.Errorf("wrong bare repo leaked the feat-x ref:\n%s", refsAfter)
	}
}
