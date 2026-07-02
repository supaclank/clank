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
	"github.com/acksell/clank/internal/host/store"
	"github.com/oklog/ulid/v2"
)

// setupRepoDeleteFixture is setupRepoFirstImport plus a sessions store,
// so busy-guard + session-purge behavior is exercised. Not parallel-
// safe (package globals).
func setupRepoDeleteFixture(t *testing.T) (*host.Service, *store.Store, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	src := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(src, "git", "init", "-b", "main")
	run(src, "git", "config", "user.email", "t@t")
	run(src, "git", "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "git", "add", ".")
	run(src, "git", "commit", "-m", "one")
	run(src, "git", "branch", "feature")

	reposRoot := t.TempDir()
	bare := filepath.Join(reposRoot, "acme", "api.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	run(reposRoot, "git", "clone", "--bare", src, bare)
	run(bare, "git", "config", "uploadpack.allowFilter", "true")

	prevBase := host.SetGitHubCloneBaseForTest("file://" + reposRoot)
	t.Cleanup(func() { host.SetGitHubCloneBaseForTest(prevBase) })
	workRoot := filepath.Join(t.TempDir(), "work")
	prevRoot := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prevRoot) })

	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)
	if err := svc.GitHub().Store().Write(githubpkg.Credentials{AccessToken: "gho_test"}); err != nil {
		t.Fatal(err)
	}
	return svc, st, workRoot
}

func TestDeleteRepo_FullCleanup(t *testing.T) {
	svc, st, workRoot := setupRepoDeleteFixture(t)
	ctx := context.Background()

	main, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	feature, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "feature")
	if err != nil {
		t.Fatalf("import feature: %v", err)
	}
	seedHostSession(t, st, ulid.Make().String(), main.WorktreeID, agent.StatusIdle)

	if err := svc.DeleteRepo(ctx, "acme__api"); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}

	for _, dir := range []string{main.WorktreeDir, feature.WorktreeDir, filepath.Join(workRoot, "repos", "acme__api")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s still present: err=%v", dir, err)
		}
	}
	sessions, err := st.ListSessionsByWorktree(ctx, main.WorktreeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("sessions survived repo delete: %d", len(sessions))
	}
	// Gone from the listing too.
	repos, err := svc.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Errorf("repos = %+v, want empty", repos)
	}
}

func TestDeleteRepo_RefusesWhenBusy(t *testing.T) {
	svc, st, workRoot := setupRepoDeleteFixture(t)
	ctx := context.Background()

	main, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	seedHostSession(t, st, ulid.Make().String(), main.WorktreeID, agent.StatusBusy)

	if err := svc.DeleteRepo(ctx, "acme__api"); !errors.Is(err, host.ErrWorktreeBusy) {
		t.Fatalf("err = %v, want ErrWorktreeBusy", err)
	}
	// Nothing deleted.
	if _, err := os.Stat(main.WorktreeDir); err != nil {
		t.Errorf("worktree removed despite busy session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workRoot, "repos", "acme__api", "repo.git")); err != nil {
		t.Errorf("canonical removed despite busy session: %v", err)
	}
}

func TestDeleteRepo_UnknownSlug(t *testing.T) {
	svc, _, _ := setupRepoDeleteFixture(t)
	if err := svc.DeleteRepo(context.Background(), "no__such"); !errors.Is(err, host.ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}
