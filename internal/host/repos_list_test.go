package host_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
)

// Not parallel: mutates the work-root/clone-base globals via the fixture.
func TestListRepos_ReposAndWorktrees(t *testing.T) {
	svc, workRoot := setupRepoFirstImport(t)
	ctx := context.Background()

	imported, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "feature"); err != nil {
		t.Fatalf("import feature: %v", err)
	}
	templateURL, _ := makeTemplateRepo(t)
	if _, err := svc.CreateProjectFromTemplate(ctx, templateURL, "", "My App"); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	repos, err := svc.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("len(repos) = %d, want 2 (import + greenfield)", len(repos))
	}
	byS := map[string]host.RepoInfo{}
	for _, r := range repos {
		byS[r.Slug] = r
	}

	api := byS["acme__api"]
	// file:// origins aren't GitHub-parseable → label falls back to the
	// stamped clank.repo-label; origin stays null.
	if api.Label != "acme/api" {
		t.Errorf("api label = %q, want acme/api", api.Label)
	}
	if api.Origin != nil {
		t.Errorf("api origin = %+v, want nil for a non-github origin URL", api.Origin)
	}
	if api.DefaultBranch != "main" {
		t.Errorf("api default branch = %q", api.DefaultBranch)
	}
	if len(api.Worktrees) != 2 {
		t.Fatalf("api worktrees = %d, want 2", len(api.Worktrees))
	}
	var sawMain bool
	for _, wt := range api.Worktrees {
		if wt.Branch == "main" {
			sawMain = true
			if wt.WorktreeID != imported.WorktreeID {
				t.Errorf("main worktree id = %s, want %s", wt.WorktreeID, imported.WorktreeID)
			}
		}
	}
	if !sawMain {
		t.Error("main branch missing from api worktrees")
	}

	app := byS["My-App"]
	if app.Label != "My App" {
		t.Errorf("greenfield label = %q, want My App", app.Label)
	}
	if app.Origin != nil {
		t.Errorf("greenfield origin = %+v, want nil", app.Origin)
	}
	if len(app.Worktrees) != 1 || app.Worktrees[0].Branch != "main" {
		t.Errorf("greenfield worktrees = %+v", app.Worktrees)
	}

	// A github-shaped origin URL flips the label + origin object.
	gitDir := filepath.Join(workRoot, "repos", "acme__api", "repo.git")
	if err := git.SetLocalConfig(gitDir, "remote.origin.url", "https://github.com/acksell/api.git"); err != nil {
		t.Fatal(err)
	}
	repos, err = svc.ListRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		if r.Slug != "acme__api" {
			continue
		}
		if r.Origin == nil || r.Origin.Owner != "acksell" || r.Origin.Repo != "api" {
			t.Errorf("origin = %+v, want acksell/api", r.Origin)
		}
		if r.Label != "acksell/api" {
			t.Errorf("label = %q, want acksell/api (origin wins over config)", r.Label)
		}
	}
}

// Not parallel: SetWorkRootForTest mutates a package-level override.
func TestListRepos_EmptyAndTornEntries(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	svc := newTestService(t)

	// No repos dir at all → empty list, no error.
	repos, err := svc.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos (no dir): %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("repos = %v, want empty", repos)
	}

	// A slug dir without repo.git (torn creation) is skipped, not fatal.
	if err := os.MkdirAll(filepath.Join(workRoot, "repos", "torn"), 0o755); err != nil {
		t.Fatal(err)
	}
	repos, err = svc.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos (torn entry): %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("repos = %v, want empty (torn dir skipped)", repos)
	}
}
