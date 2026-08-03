package host_test

// CreateRepoWorktree (POST /repos/{slug}/worktrees) — the repo-scoped
// replacement for CreateWorktree. Ports create_worktree_fetch_test.go's
// scenarios to repo scope: the base/loaded branch may be local, cloned,
// or remote-only (fetched on demand), which retires the shallow-clone
// 404 class.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/supaclank/clank/internal/git"
	"github.com/supaclank/clank/internal/host"
)

func TestCreateRepoWorktree_Validation(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()

	cases := []struct {
		name string
		slug string
		req  host.RepoWorktreeRequest
		want error
	}{
		{"neither field", "acme__api", host.RepoWorktreeRequest{}, host.ErrInvalidArgument},
		{"both fields", "acme__api", host.RepoWorktreeRequest{Branch: "a", BaseBranch: "b"}, host.ErrInvalidArgument},
		{"flag-like branch", "acme__api", host.RepoWorktreeRequest{Branch: "-evil"}, host.ErrInvalidArgument},
		{"leading space branch", "acme__api", host.RepoWorktreeRequest{Branch: "  main"}, host.ErrInvalidArgument},
		{"trailing space branch", "acme__api", host.RepoWorktreeRequest{Branch: "main  "}, host.ErrInvalidArgument},
		{"tab in branch", "acme__api", host.RepoWorktreeRequest{Branch: "main\ttab"}, host.ErrInvalidArgument},
		{"newline in branch", "acme__api", host.RepoWorktreeRequest{BaseBranch: "main\nfeature"}, host.ErrInvalidArgument},
		{"traversal slug", "..", host.RepoWorktreeRequest{Branch: "main"}, host.ErrInvalidArgument},
		{"slash slug", "a/b", host.RepoWorktreeRequest{Branch: "main"}, host.ErrInvalidArgument},
		{"empty slug", "", host.RepoWorktreeRequest{Branch: "main"}, host.ErrInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateRepoWorktree(ctx, tc.slug, tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// Not parallel: mutates the work-root/clone-base globals via the fixture.
func TestCreateRepoWorktree_UnknownRepo(t *testing.T) {
	svc, _ := setupRepoFirstImport(t)
	_, err := svc.CreateRepoWorktree(context.Background(), "no__such", host.RepoWorktreeRequest{Branch: "main"})
	if !errors.Is(err, host.ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

// Fork off a LOADED branch: the canonical has refs/heads/main live in a
// worktree; the fork bases on that live tip.
func TestCreateRepoWorktree_ForkFromLoadedBranch(t *testing.T) {
	svc, workRoot := setupRepoFirstImport(t)
	ctx := context.Background()

	if _, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main"); err != nil {
		t.Fatalf("import: %v", err)
	}
	res, err := svc.CreateRepoWorktree(ctx, "acme__api", host.RepoWorktreeRequest{BaseBranch: "main"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if !res.Created {
		t.Error("fork returned created=false")
	}
	if res.Branch == "" || res.Branch == "main" {
		t.Errorf("fork branch = %q, want a fresh petname", res.Branch)
	}
	if res.RepoSlug != "acme__api" {
		t.Errorf("repo slug = %q", res.RepoSlug)
	}
	// THE bug this surface retires: the returned worktree dir is exactly
	// where session GitRefs resolve (~/work/<id>), not ~/.clank/worktrees.
	if want := filepath.Join(workRoot, res.WorktreeID); res.WorktreeDir != want {
		t.Errorf("worktree dir = %q, want %q", res.WorktreeDir, want)
	}
	if _, err := os.Stat(res.WorktreeDir); err != nil {
		t.Errorf("worktree dir missing: %v", err)
	}
}

// Fork off a REMOTE-ONLY base: feature exists on origin but the
// canonical (single-branch clone of main) has no ref for it — the fork
// fetches it on demand. This is the ported "fetch missing base" test.
func TestCreateRepoWorktree_ForkFromRemoteOnlyBase(t *testing.T) {
	svc, _ := setupRepoFirstImport(t)
	ctx := context.Background()

	if _, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main"); err != nil {
		t.Fatalf("import: %v", err)
	}
	res, err := svc.CreateRepoWorktree(ctx, "acme__api", host.RepoWorktreeRequest{BaseBranch: "feature"})
	if err != nil {
		t.Fatalf("fork from remote-only base: %v", err)
	}
	if !res.Created {
		t.Error("created = false")
	}
}

// Fork off a base that exists NOWHERE → ErrNotFound (not a transport
// error, not a 500).
func TestCreateRepoWorktree_ForkBaseAbsentEverywhere(t *testing.T) {
	svc, _ := setupRepoFirstImport(t)
	ctx := context.Background()

	if _, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main"); err != nil {
		t.Fatalf("import: %v", err)
	}
	_, err := svc.CreateRepoWorktree(ctx, "acme__api", host.RepoWorktreeRequest{BaseBranch: "ghost"})
	if !errors.Is(err, host.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// LOAD a remote-only branch: fetched, local ref created, worktree
// linked. Loading it AGAIN returns the same worktree with created=false.
func TestCreateRepoWorktree_LoadRemoteBranchIdempotent(t *testing.T) {
	svc, _ := setupRepoFirstImport(t)
	ctx := context.Background()

	if _, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main"); err != nil {
		t.Fatalf("import: %v", err)
	}
	first, err := svc.CreateRepoWorktree(ctx, "acme__api", host.RepoWorktreeRequest{Branch: "feature"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !first.Created || first.Branch != "feature" {
		t.Errorf("load = created:%v branch:%q, want created:true feature", first.Created, first.Branch)
	}
	again, err := svc.CreateRepoWorktree(ctx, "acme__api", host.RepoWorktreeRequest{Branch: "feature"})
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if again.Created {
		t.Error("re-load created a duplicate worktree")
	}
	if again.WorktreeID != first.WorktreeID {
		t.Errorf("re-load worktree = %s, want %s", again.WorktreeID, first.WorktreeID)
	}
}

// Fork off an UNLOADED-BUT-LOCAL base: load feature, delete its
// worktree (branch ref survives by design), fork off the surviving ref.
func TestCreateRepoWorktree_ForkFromUnloadedLocalBranch(t *testing.T) {
	svc, _ := setupRepoFirstImport(t)
	ctx := context.Background()

	if _, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main"); err != nil {
		t.Fatalf("import: %v", err)
	}
	loaded, err := svc.CreateRepoWorktree(ctx, "acme__api", host.RepoWorktreeRequest{Branch: "feature"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := svc.DeleteWorktree(ctx, loaded.WorktreeID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	res, err := svc.CreateRepoWorktree(ctx, "acme__api", host.RepoWorktreeRequest{BaseBranch: "feature"})
	if err != nil {
		t.Fatalf("fork off surviving local ref: %v", err)
	}
	if !res.Created {
		t.Error("created = false")
	}
}

// GREENFIELD OFFLINE: a scaffolded repo (no origin, no token needed)
// forks off its own main without any network.
func TestCreateRepoWorktree_GreenfieldOfflineFork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workRoot := filepath.Join(t.TempDir(), "work")
	prevRoot := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prevRoot) })

	templateURL, _ := makeTemplateRepo(t)
	svc := newTestService(t)

	created, err := svc.CreateProjectFromTemplate(context.Background(), templateURL, "", "My App")
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	res, err := svc.CreateRepoWorktree(context.Background(), created.RepoSlug, host.RepoWorktreeRequest{BaseBranch: "main"})
	if err != nil {
		t.Fatalf("greenfield fork: %v", err)
	}
	if !res.Created {
		t.Error("created = false")
	}
	// Both branches loaded now: main + the fork.
	gitDir := filepath.Join(workRoot, "repos", "My-App", "repo.git")
	wts, err := git.ListWorktrees(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	var linked int
	for _, wt := range wts {
		if !wt.Bare {
			linked++
		}
	}
	if linked != 2 {
		t.Errorf("linked worktrees = %d, want 2", linked)
	}
}
