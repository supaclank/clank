package host_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// The git half: loaded/dirty/ahead/behind derived locally, ?fetch=1
// picking up remote movement. file:// origin → Origin nil → no PR half.
// Not parallel: mutates package globals via the fixture.
func TestRepoOverview_GitHalf(t *testing.T) {
	svc, workRoot := setupRepoFirstImport(t)
	ctx := context.Background()

	imported, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main")
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// Baseline: main loaded + clean, feature exists remotely only (not listed).
	ov, err := svc.RepoOverview(ctx, "acme__api", false)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.DefaultBranch != "main" || ov.Origin != nil {
		t.Errorf("default=%q origin=%+v, want main/nil", ov.DefaultBranch, ov.Origin)
	}
	if len(ov.Branches) != 1 {
		t.Fatalf("branches = %+v, want just main (feature is remote-only)", ov.Branches)
	}
	main := ov.Branches[0]
	if main.Branch != "main" || !main.Loaded || main.WorktreeID != imported.WorktreeID || main.Dirty {
		t.Errorf("main = %+v, want loaded+clean with worktree %s", main, imported.WorktreeID)
	}
	if main.Ahead == nil || main.Behind == nil || *main.Ahead != 0 || *main.Behind != 0 {
		t.Errorf("main ahead/behind = %v/%v, want 0/0", main.Ahead, main.Behind)
	}
	if main.LastCommitAt.IsZero() {
		t.Error("main.LastCommitAt is zero")
	}

	// Dirty: uncommitted file in the worktree.
	if err := os.WriteFile(filepath.Join(imported.WorktreeDir, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ahead: a commit in the worktree moves refs/heads/main past origin/main.
	runGitIn(t, imported.WorktreeDir, "add", ".")
	runGitIn(t, imported.WorktreeDir, "commit", "-m", "local work")
	if err := os.WriteFile(filepath.Join(imported.WorktreeDir, "wip2.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ov, err = svc.RepoOverview(ctx, "acme__api", false)
	if err != nil {
		t.Fatal(err)
	}
	main = ov.Branches[0]
	if !main.Dirty {
		t.Error("main.Dirty = false after uncommitted file")
	}
	if main.Ahead == nil || *main.Ahead != 1 {
		t.Errorf("main.Ahead = %v, want 1 after local commit", main.Ahead)
	}

	// Behind: advance the ORIGIN, invisible without fetch, visible with.
	gitDir := filepath.Join(workRoot, "repos", "acme__api", "repo.git")
	originURL, err := git.RemoteURL(gitDir, "origin")
	if err != nil {
		t.Fatal(err)
	}
	advanceBareOrigin(t, strings.TrimPrefix(originURL, "file://"))

	ov, err = svc.RepoOverview(ctx, "acme__api", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := *ov.Branches[0].Behind; got != 0 {
		t.Errorf("behind (no fetch) = %d, want 0 (stale tracking ref)", got)
	}
	if ov.Fetched {
		t.Error("Fetched = true without ?fetch=1")
	}

	ov, err = svc.RepoOverview(ctx, "acme__api", true)
	if err != nil {
		t.Fatalf("overview fetch=1: %v", err)
	}
	if !ov.Fetched {
		t.Error("Fetched = false with ?fetch=1")
	}
	if got := *ov.Branches[0].Behind; got != 1 {
		t.Errorf("behind (fetched) = %d, want 1", got)
	}
}

// The PR half: open PRs merge onto matching branches; PR-only heads
// appear loaded:false; is_mine keys on the connected login; an API
// failure degrades to the git half. Not parallel (fixture globals).
func TestRepoOverview_PRHalf(t *testing.T) {
	svc, workRoot := setupRepoFirstImport(t)
	ctx := context.Background()

	if _, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main"); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Stub the GitHub API and re-point the canonical's origin at a
	// github-shaped URL so Origin parses. No ?fetch=1 in this test — the
	// URL is fake; only the API base is real.
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acksell/api/pulls" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":7,"title":"My PR","state":"open","draft":false,
			 "html_url":"https://github.com/acksell/api/pull/7",
			 "head":{"ref":"main"},"base":{"ref":"main"},
			 "user":{"login":"acksell"},"updated_at":"2026-07-01T10:00:00Z"},
			{"number":8,"title":"Colleague PR","state":"open","draft":true,
			 "html_url":"https://github.com/acksell/api/pull/8",
			 "head":{"ref":"colleagues-branch"},"base":{"ref":"main"},
			 "user":{"login":"alice"},"updated_at":"2026-07-01T09:00:00Z"}
		]`))
	}))
	t.Cleanup(apiSrv.Close)
	svc.GitHub().SetAPIBaseURL(apiSrv.URL)
	if err := svc.GitHub().Store().Write(githubpkg.Credentials{AccessToken: "gho_test", GitHubLogin: "acksell"}); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(workRoot, "repos", "acme__api", "repo.git")
	if err := git.SetLocalConfig(gitDir, "remote.origin.url", "https://github.com/acksell/api.git"); err != nil {
		t.Fatal(err)
	}

	ov, err := svc.RepoOverview(ctx, "acme__api", false)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.Origin == nil || ov.Origin.Owner != "acksell" {
		t.Fatalf("origin = %+v", ov.Origin)
	}
	byBranch := map[string]host.RepoBranchOverview{}
	for _, b := range ov.Branches {
		byBranch[b.Branch] = b
	}
	main := byBranch["main"]
	if main.PR == nil || main.PR.Number != 7 || !main.PR.IsMine {
		t.Errorf("main.PR = %+v, want #7 is_mine", main.PR)
	}
	colleague, ok := byBranch["colleagues-branch"]
	if !ok {
		t.Fatal("PR-only head missing from overview")
	}
	if colleague.Loaded || colleague.PR == nil || colleague.PR.Number != 8 || colleague.PR.IsMine || !colleague.PR.Draft {
		t.Errorf("colleague entry = %+v, want unloaded draft PR #8 by alice", colleague)
	}

	// GitHub down → git half intact, no PR annotations, no error.
	apiSrv.Close()
	ov, err = svc.RepoOverview(ctx, "acme__api", false)
	if err != nil {
		t.Fatalf("overview with API down: %v", err)
	}
	if _, ok := ovBranch(ov, "colleagues-branch"); ok {
		t.Error("PR-only head present despite API failure")
	}
	if m, ok := ovBranch(ov, "main"); !ok || m.PR != nil {
		t.Errorf("main = %+v, want present without PR annotation", m)
	}
}

// PR-only heads are merged in via a map iteration (random order in Go);
// the result must still come out sorted most-recently-active first,
// matching LocalBranchTips' documented order. Not parallel (fixture
// globals).
func TestRepoOverview_PRHalfSortedByRecency(t *testing.T) {
	svc, workRoot := setupRepoFirstImport(t)
	ctx := context.Background()

	if _, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main"); err != nil {
		t.Fatalf("import: %v", err)
	}

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acksell/api/pulls" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":10,"title":"Oldest","state":"open","draft":false,
			 "html_url":"https://github.com/acksell/api/pull/10",
			 "head":{"ref":"branch-oldest"},"base":{"ref":"main"},
			 "user":{"login":"alice"},"updated_at":"2026-06-30T00:00:00Z"},
			{"number":11,"title":"Newest","state":"open","draft":false,
			 "html_url":"https://github.com/acksell/api/pull/11",
			 "head":{"ref":"branch-newest"},"base":{"ref":"main"},
			 "user":{"login":"alice"},"updated_at":"2026-07-02T00:00:00Z"},
			{"number":12,"title":"Middle","state":"open","draft":false,
			 "html_url":"https://github.com/acksell/api/pull/12",
			 "head":{"ref":"branch-middle"},"base":{"ref":"main"},
			 "user":{"login":"alice"},"updated_at":"2026-07-01T00:00:00Z"}
		]`))
	}))
	t.Cleanup(apiSrv.Close)
	svc.GitHub().SetAPIBaseURL(apiSrv.URL)
	if err := svc.GitHub().Store().Write(githubpkg.Credentials{AccessToken: "gho_test", GitHubLogin: "acksell"}); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(workRoot, "repos", "acme__api", "repo.git")
	if err := git.SetLocalConfig(gitDir, "remote.origin.url", "https://github.com/acksell/api.git"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		ov, err := svc.RepoOverview(ctx, "acme__api", false)
		if err != nil {
			t.Fatalf("overview: %v", err)
		}
		var prOnly []string
		for _, b := range ov.Branches {
			if !b.Loaded {
				prOnly = append(prOnly, b.Branch)
			}
		}
		want := []string{"branch-newest", "branch-middle", "branch-oldest"}
		if strings.Join(prOnly, ",") != strings.Join(want, ",") {
			t.Fatalf("iteration %d: PR-only branches = %v, want %v", i, prOnly, want)
		}
	}
}

// Unknown slug → ErrRepoNotFound; greenfield → origin null, main loaded.
// Not parallel (fixture globals).
func TestRepoOverview_GreenfieldAndUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	templateURL, _ := makeTemplateRepo(t)
	svc := newTestService(t)

	if _, err := svc.RepoOverview(context.Background(), "nope", false); err == nil {
		t.Fatal("unknown slug: err = nil, want ErrRepoNotFound")
	}

	created, err := svc.CreateProjectFromTemplate(context.Background(), templateURL, "Solo")
	if err != nil {
		t.Fatal(err)
	}
	ov, err := svc.RepoOverview(context.Background(), created.RepoSlug, false)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Origin != nil || len(ov.Branches) != 1 || !ov.Branches[0].Loaded {
		t.Errorf("greenfield overview = %+v", ov)
	}
	if ov.Branches[0].Ahead != nil {
		t.Errorf("greenfield ahead = %v, want nil (no tracking ref)", ov.Branches[0].Ahead)
	}
}

// ovBranch finds a branch entry by name.
func ovBranch(ov host.RepoOverviewResult, branch string) (host.RepoBranchOverview, bool) {
	for _, b := range ov.Branches {
		if b.Branch == branch {
			return b, true
		}
	}
	return host.RepoBranchOverview{}, false
}

// runGitIn runs git in dir, failing the test on error.
func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// advanceBareOrigin adds one commit to the bare origin at barePath by
// committing in a scratch clone and pushing back.
func advanceBareOrigin(t *testing.T, barePath string) {
	t.Helper()
	scratch := t.TempDir()
	runGitIn(t, filepath.Dir(scratch), "clone", barePath, filepath.Join(scratch, "c"))
	c := filepath.Join(scratch, "c")
	runGitIn(t, c, "config", "user.email", "t@t")
	runGitIn(t, c, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(c, "remote.txt"), []byte("remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, c, "add", ".")
	runGitIn(t, c, "commit", "-m", "remote work")
	runGitIn(t, c, "push", "origin", "main")
}
