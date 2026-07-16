package host_test

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
	hoststore "github.com/acksell/clank/internal/host/store"
)

// gitIn runs a git command in dir, failing the test on error and
// returning trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newLocalRepoService builds a Service wired with a real sessions store
// and seeds one session row per project dir, so discoveredLocalRepos
// has history to mine.
func newLocalRepoService(t *testing.T, projectDirs ...string) *host.Service {
	t.Helper()
	st, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("open sessions store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)
	// Explicit increasing timestamps: recency ordering must be
	// deterministic, and same-millisecond seeds would tie-break
	// arbitrarily.
	base := time.Now().Add(-time.Hour)
	for i, dir := range projectDirs {
		info := agent.SessionInfo{
			ID:        "01SESSION" + string(rune('A'+i)),
			Backend:   agent.BackendOpenCode,
			Status:    agent.StatusIdle,
			GitRef:    agent.GitRef{LocalPath: dir},
			UpdatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := st.UpsertSession(context.Background(), info); err != nil {
			t.Fatalf("seed session for %s: %v", dir, err)
		}
	}
	return svc
}

// localSlugFor mirrors the slug encoding so tests can address a
// checkout the way clients do: base64url of the identity root — the
// MAIN worktree's root, symlink-resolved (the same resolution
// discovery uses).
func localSlugFor(t *testing.T, dir string) (slug, root string) {
	t.Helper()
	root, err := git.MainWorktreeRoot(dir)
	if err != nil {
		t.Fatalf("MainWorktreeRoot(%s): %v", dir, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", root, err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(root)), root
}

func TestListRepos_DiscoversLocalCheckouts(t *testing.T) {
	// Not parallel: SetWorkRootForTest mutates a global.
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	repoA := initGitRepo(t, "git@github.com:acme/widget.git")
	repoB := initGitRepo(t, "https://example.com/things/gadget.git")
	if err := os.MkdirAll(filepath.Join(repoB, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A linked worktree of repoA, the way months of TUI sessions leave
	// them behind — it must collapse into repoA, not list as its own
	// repo (show-toplevel reports a worktree's own path as its root).
	wtA := filepath.Join(t.TempDir(), "wt-side")
	gitIn(t, repoA, "worktree", "add", "-b", "side", wtA)

	// A bare repo (old sync-era mirror shape): not a checkout, skipped.
	bareDir := filepath.Join(t.TempDir(), "mirror.git")
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}

	plainDir := t.TempDir()                            // session outside any repo
	goneDir := filepath.Join(t.TempDir(), "vanished")  // folder deleted since
	workDir := filepath.Join(workRoot, "01WORKTREEXX") // canonical worktree — already listed via its repo

	svc := newLocalRepoService(t,
		repoA,
		repoA,                       // duplicate session on the same repo
		wtA,                         // linked worktree — same repo as repoA
		filepath.Join(repoB, "sub"), // subdir resolves to repoB's root
		bareDir,
		plainDir,
		goneDir,
		workDir,
	)

	repos, err := svc.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("repos = %d (%+v), want 2 discovered checkouts", len(repos), repos)
	}

	// Recency order (IDE recents): repoB's session was seeded last, so
	// it's the most recently updated and must list first.
	_, wantFirst := localSlugFor(t, repoB)
	if repos[0].Path != wantFirst {
		t.Errorf("repos[0].Path = %q, want most-recently-used %q", repos[0].Path, wantFirst)
	}

	byPath := map[string]host.RepoInfo{}
	for _, r := range repos {
		if !r.IsLocalCheckout {
			t.Errorf("repo %s: IsLocalCheckout = false, want true", r.Slug)
		}
		byPath[r.Path] = r
	}

	slugA, rootA := localSlugFor(t, repoA)
	a, ok := byPath[rootA]
	if !ok {
		t.Fatalf("repoA %s missing from listing: %+v", rootA, repos)
	}
	if a.Slug != slugA {
		t.Errorf("slug = %q, want %q (base64url of root)", a.Slug, slugA)
	}
	if a.Origin == nil || a.Origin.Owner != "acme" || a.Origin.Repo != "widget" {
		t.Errorf("origin = %+v, want acme/widget", a.Origin)
	}
	if a.Label != "acme/widget" || a.DefaultBranch != "main" || len(a.Worktrees) != 0 {
		t.Errorf("label=%q default=%q worktrees=%d, want acme/widget / main / 0", a.Label, a.DefaultBranch, len(a.Worktrees))
	}

	_, rootB := localSlugFor(t, repoB)
	b, ok := byPath[rootB]
	if !ok {
		t.Fatalf("repoB %s missing from listing: %+v", rootB, repos)
	}
	// Non-GitHub origin: still listed (it's a real local repo), just
	// unpublished as far as GitHub flows go.
	if b.Origin != nil {
		t.Errorf("origin = %+v, want nil for non-github remote", b.Origin)
	}
	if b.Label != "things/gadget" {
		t.Errorf("label = %q, want things/gadget", b.Label)
	}
}

// TestListRepos_SymlinkedWorkRootExcludesInternalCheckouts pins the
// macOS /tmp-vs-/private/tmp class of bug: localCheckoutRoot
// symlink-resolves every candidate root before the containment check
// runs, but workRootDir() returns the unresolved spelling. A session
// whose recorded path already names clank's work directory through its
// resolved (non-alias) form — e.g. because something upstream resolved
// symlinks before persisting it — bypasses the raw-string pre-filter and
// then, since the resolved root can't match the still-unresolved
// workRoot either, leaks into the listing as a "local repo".
func TestListRepos_SymlinkedWorkRootExcludesInternalCheckouts(t *testing.T) {
	// Not parallel: SetWorkRootForTest mutates a global.
	realWorkRoot := t.TempDir()
	symlinkedWorkRoot := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(realWorkRoot, symlinkedWorkRoot); err != nil {
		t.Fatalf("symlink work root: %v", err)
	}
	prev := host.SetWorkRootForTest(symlinkedWorkRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	// A worktree dir created directly under the real (resolved) root —
	// same location clank's own worktrees live in, just named through
	// its resolved spelling rather than the workRootDir() alias.
	internalWorktree := filepath.Join(realWorkRoot, "01WORKTREEYY")
	if err := os.MkdirAll(internalWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = internalWorktree
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(internalWorktree, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "initial")

	svc := newLocalRepoService(t, internalWorktree)

	repos, err := svc.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	for _, r := range repos {
		if r.Path == realWorkRoot || strings.HasPrefix(r.Path, realWorkRoot+string(filepath.Separator)) {
			t.Errorf("clank-owned worktree %q leaked into discovered local repos: %+v", r.Path, repos)
		}
	}
}

func TestCreateRepoWorktree_LocalCheckoutForkAndLoad(t *testing.T) {
	// Not parallel: SetWorkRootForTest mutates a global.
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	source := initGitRepo(t, "git@github.com:acme/widget.git")
	gitIn(t, source, "branch", "feature") // exists, not checked out

	svc := newLocalRepoService(t, source)
	slug, root := localSlugFor(t, source)

	// FORK off main: worktree under ~/work, stamped, petname branch.
	forked, err := svc.CreateRepoWorktree(context.Background(), slug, host.RepoWorktreeRequest{BaseBranch: "main"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if !forked.Created || forked.WorktreeID == "" {
		t.Fatalf("fork result = %+v, want created worktree", forked)
	}
	if got := filepath.Dir(forked.WorktreeDir); got != workRoot {
		t.Errorf("worktree parent = %q, want %q", got, workRoot)
	}
	if forked.RepoSlug != slug {
		t.Errorf("result slug = %q, want %q", forked.RepoSlug, slug)
	}
	// Live sharing: the fork's branch is a ref of the USER's repo.
	if exists, err := git.BranchExists(root, forked.Branch); err != nil || !exists {
		t.Errorf("branch %q not visible in the user's checkout (exists=%v err=%v)", forked.Branch, exists, err)
	}
	// The user's own checkout is untouched.
	if head := gitIn(t, root, "symbolic-ref", "--short", "HEAD"); head != "main" {
		t.Errorf("user checkout HEAD = %q, want main", head)
	}

	// LOAD the existing feature branch; idempotent on repeat.
	loaded, err := svc.CreateRepoWorktree(context.Background(), slug, host.RepoWorktreeRequest{Branch: "feature"})
	if err != nil {
		t.Fatalf("load feature: %v", err)
	}
	if !loaded.Created || loaded.Branch != "feature" {
		t.Fatalf("load result = %+v, want created feature worktree", loaded)
	}
	again, err := svc.CreateRepoWorktree(context.Background(), slug, host.RepoWorktreeRequest{Branch: "feature"})
	if err != nil {
		t.Fatalf("re-load feature: %v", err)
	}
	if again.Created || again.WorktreeID != loaded.WorktreeID {
		t.Errorf("re-load = %+v, want idempotent hit on %s", again, loaded.WorktreeID)
	}

	// Listing now shows both clank worktrees — and never the primary.
	repos, err := svc.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != 1 || len(repos[0].Worktrees) != 2 {
		t.Fatalf("listing = %+v, want 1 repo with 2 worktrees", repos)
	}

	// The default branch is the user's primary checkout — reserved.
	if _, err := svc.CreateRepoWorktree(context.Background(), slug, host.RepoWorktreeRequest{Branch: "main"}); !errors.Is(err, host.ErrReservedBranch) {
		t.Errorf("load main err = %v, want ErrReservedBranch", err)
	}

	// A branch checked out in the user's primary worktree can't be
	// loaded (git allows one checkout per branch) — typed conflict.
	gitIn(t, root, "checkout", "-b", "wip")
	if _, err := svc.CreateRepoWorktree(context.Background(), slug, host.RepoWorktreeRequest{Branch: "wip"}); !errors.Is(err, host.ErrBranchCheckedOutElsewhere) {
		t.Errorf("load wip err = %v, want ErrBranchCheckedOutElsewhere", err)
	}
}

func TestDeleteRepo_RefusesLocalCheckout(t *testing.T) {
	// Not parallel: SetWorkRootForTest mutates a global.
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	source := initGitRepo(t, "git@github.com:acme/widget.git")
	svc := newLocalRepoService(t, source)
	slug, root := localSlugFor(t, source)

	if err := svc.DeleteRepo(context.Background(), slug); !errors.Is(err, host.ErrCannotDeleteLocalCheckout) {
		t.Fatalf("err = %v, want ErrCannotDeleteLocalCheckout", err)
	}
	if _, err := os.Stat(filepath.Join(root, "README")); err != nil {
		t.Fatalf("user checkout was touched: %v", err)
	}
}

func TestResolveRepoSlug_CanonicalWinsOverLocalDecoding(t *testing.T) {
	// Not parallel: SetWorkRootForTest mutates a global.
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	source := initGitRepo(t, "git@github.com:acme/widget.git")
	svc := newLocalRepoService(t, source)
	slug, root := localSlugFor(t, source)

	// Adversarial layout: a canonical whose dir name IS the local slug.
	// Resolution must prefer it — so ops (including delete) target the
	// canonical, never the user's folder.
	canonical := filepath.Join(workRoot, "repos", slug, "repo.git")
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", canonical).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}

	if err := svc.DeleteRepo(context.Background(), slug); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Errorf("canonical still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "README")); err != nil {
		t.Errorf("user checkout was touched: %v", err)
	}
}

func TestResolveRepoSlug_LongLocalPathFallsBackToLocalDecoding(t *testing.T) {
	// Not parallel: SetWorkRootForTest mutates a global.
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	// ~/work/repos must exist for the canonical stat to actually reach
	// (and fail on) the long component — on any host with at least one
	// canonical it already does; an absent parent dir would fail at
	// ENOENT before the length is ever evaluated.
	if err := os.MkdirAll(filepath.Join(workRoot, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Build a root whose base64url slug exceeds a single path
	// component's NAME_MAX (255 bytes on ext4/APFS) — regression for
	// resolveRepoSlug treating the resulting ENAMETOOLONG canonical
	// stat as a hard error instead of falling through to local
	// decoding (base64 inflates length by ~4/3, so ~200 raw bytes is
	// enough to cross 255 encoded).
	long := t.TempDir()
	for len(long) < 220 {
		long = filepath.Join(long, strings.Repeat("x", 40))
	}
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, long, "init", "-b", "main")
	gitIn(t, long, "config", "user.email", "t@t")
	gitIn(t, long, "config", "user.name", "T")
	gitIn(t, long, "remote", "add", "origin", "git@github.com:acme/widget.git")
	if err := os.WriteFile(filepath.Join(long, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, long, "add", ".")
	gitIn(t, long, "commit", "-m", "initial")

	svc := newLocalRepoService(t, long)
	slug, _ := localSlugFor(t, long)
	if len(slug) <= 255 {
		t.Fatalf("test setup: slug length = %d, want > 255 to exercise ENAMETOOLONG", len(slug))
	}

	overview, err := svc.RepoOverview(context.Background(), slug, false)
	if err != nil {
		t.Fatalf("RepoOverview: %v", err)
	}
	if overview.Slug != slug {
		t.Errorf("slug = %q, want %q", overview.Slug, slug)
	}
}

func TestListRepos_CapsDiscoveredLocalRepos(t *testing.T) {
	// Not parallel: SetWorkRootForTest mutates a global.
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	const total = host.MaxDiscoveredLocalReposForTest + 5
	dirs := make([]string, total)
	for i := range dirs {
		dirs[i] = initGitRepo(t, "git@github.com:acme/widget.git")
	}

	svc := newLocalRepoService(t, dirs...)
	repos, err := svc.ListRepos(context.Background())
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if len(repos) != host.MaxDiscoveredLocalReposForTest {
		t.Fatalf("repos = %d, want capped at %d", len(repos), host.MaxDiscoveredLocalReposForTest)
	}
}

func TestRepoOverview_LocalCheckout(t *testing.T) {
	// Not parallel: SetWorkRootForTest + t.Setenv mutate globals.
	t.Setenv("HOME", t.TempDir())
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	source := initGitRepo(t, "git@github.com:acme/widget.git")
	// The user is off on a feature branch; the overview's default must
	// still be the probed default, not their current HEAD.
	gitIn(t, source, "checkout", "-b", "wip")

	svc := newLocalRepoService(t, source)
	slug, _ := localSlugFor(t, source)

	overview, err := svc.RepoOverview(context.Background(), slug, false)
	if err != nil {
		t.Fatalf("RepoOverview: %v", err)
	}
	if overview.Slug != slug || overview.DefaultBranch != "main" {
		t.Errorf("slug=%q default=%q, want %q / main", overview.Slug, overview.DefaultBranch, slug)
	}
	branches := map[string]bool{}
	for _, b := range overview.Branches {
		branches[b.Branch] = true
	}
	if !branches["main"] || !branches["wip"] {
		t.Errorf("branches = %v, want the user's real refs/heads (main, wip)", branches)
	}
}

func TestRepoOverview_LocalCheckoutNoOriginLabelsByRepoName(t *testing.T) {
	// Not parallel: SetWorkRootForTest mutates a global.
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	// Greenfield local repo, no origin yet. repoLabelFor's fallback
	// (parent-dir name, correct for a canonical's <slug>/repo.git
	// shape) must not leak into a checkout's label — a checkout's
	// gitDir IS the repo root, so the parent dir is the OWNER, not the
	// repo. Same shape ListRepos already labels correctly via
	// repolabel.ComputeRepoLabel.
	source := filepath.Join(t.TempDir(), "acme", "widget")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, source, "init", "-b", "main")
	gitIn(t, source, "config", "user.email", "t@t")
	gitIn(t, source, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(source, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, source, "add", ".")
	gitIn(t, source, "commit", "-m", "initial")

	svc := newLocalRepoService(t, source)
	slug, _ := localSlugFor(t, source)

	overview, err := svc.RepoOverview(context.Background(), slug, false)
	if err != nil {
		t.Fatalf("RepoOverview: %v", err)
	}
	if overview.Label != "widget" {
		t.Errorf("label = %q, want %q (repo basename, not parent/owner dir)", overview.Label, "widget")
	}

	forked, err := svc.CreateRepoWorktree(context.Background(), slug, host.RepoWorktreeRequest{BaseBranch: "main"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if forked.OriginRepo != "widget" {
		t.Errorf("fork OriginRepo = %q, want %q", forked.OriginRepo, "widget")
	}
}

func TestResolveRepoSlug_PathMaxLengthSlugIsValid(t *testing.T) {
	// Not parallel: SetWorkRootForTest mutates a global.
	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	if err := os.MkdirAll(filepath.Join(workRoot, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A deep-but-legitimate checkout path: base64 inflates length by
	// ~4/3, so ~1100 raw bytes crosses the old 1400-char slug cap while
	// staying well under a Linux PATH_MAX (4096) path's ~5460-char slug.
	// Regression for validSlug rejecting a slug ListRepos would have
	// happily returned.
	long := t.TempDir()
	for len(long) < 1100 {
		long = filepath.Join(long, strings.Repeat("x", 40))
	}
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, long, "init", "-b", "main")
	gitIn(t, long, "config", "user.email", "t@t")
	gitIn(t, long, "config", "user.name", "T")
	gitIn(t, long, "remote", "add", "origin", "git@github.com:acme/widget.git")
	if err := os.WriteFile(filepath.Join(long, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, long, "add", ".")
	gitIn(t, long, "commit", "-m", "initial")

	svc := newLocalRepoService(t, long)
	slug, _ := localSlugFor(t, long)
	if len(slug) <= 1400 {
		t.Fatalf("test setup: slug length = %d, want > 1400 to exercise the old cap", len(slug))
	}

	overview, err := svc.RepoOverview(context.Background(), slug, false)
	if err != nil {
		t.Fatalf("RepoOverview: %v", err)
	}
	if overview.Slug != slug {
		t.Errorf("slug = %q, want %q", overview.Slug, slug)
	}
}
