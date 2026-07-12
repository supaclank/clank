package host_test

// Repo-first storage behavior: one bare blobless canonical per repo at
// ~/work/repos/<slug>/repo.git + flat linked worktrees at ~/work/<ULID>.
// These tests drive the real import/scaffold/delete flows against local
// file:// origins (uploadpack.allowFilter=true makes --filter honest).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// setupRepoFirstImport wires the globals for an import test: a local
// origin with main+feature branches served at <base>/acme/api.git, a
// fresh work root, HOME with a connected token store. Returns the
// service and the work root. Not parallel-safe (package globals).
func setupRepoFirstImport(t *testing.T) (*host.Service, string) {
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
	// file:// forces the pack protocol; the server side must allow
	// filters for the blobless clone to actually filter.
	run(bare, "git", "config", "uploadpack.allowFilter", "true")

	prevBase := host.SetGitHubCloneBaseForTest("file://" + reposRoot)
	t.Cleanup(func() { host.SetGitHubCloneBaseForTest(prevBase) })

	workRoot := filepath.Join(t.TempDir(), "work")
	prevRoot := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prevRoot) })

	svc := newTestService(t)
	if err := svc.GitHub().Store().Write(githubpkg.Credentials{AccessToken: "gho_test"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	return svc, workRoot
}

// TestImport_RepoFirstLayout: importing creates the bare blobless
// canonical (with fetch refspec, credential helper, label) plus a flat
// linked worktree whose .git is a file pointing into the canonical.
func TestImport_RepoFirstLayout(t *testing.T) {
	svc, workRoot := setupRepoFirstImport(t)

	res, err := svc.ImportProjectFromGitHub(context.Background(), "acme", "api", "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.RepoSlug != "acme__api" {
		t.Errorf("repo slug = %q, want acme__api", res.RepoSlug)
	}

	gitDir := filepath.Join(workRoot, "repos", "acme__api", "repo.git")
	for key, want := range map[string]string{
		"remote.origin.promisor":           "true",
		"remote.origin.partialclonefilter": "blob:none",
		"remote.origin.fetch":              "+refs/heads/*:refs/remotes/origin/*",
		"clank.repo-label":                 "acme/api",
	} {
		got, err := git.GetLocalConfig(gitDir, key)
		if err != nil {
			t.Fatalf("GetLocalConfig(%s): %v", key, err)
		}
		if got != want {
			t.Errorf("canonical config %s = %q, want %q", key, got, want)
		}
	}
	helper, err := git.GetLocalConfig(gitDir, "credential.helper")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(helper, "git-credential") {
		t.Errorf("credential.helper = %q, want the git-credential subcommand", helper)
	}

	// The worktree is LINKED: .git is a gitdir-pointer file, not a dir.
	fi, err := os.Lstat(filepath.Join(res.WorktreeDir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.IsDir() {
		t.Error("worktree .git is a directory — expected a linked worktree gitdir file")
	}
}

// TestImport_IdempotentPerBranch: re-importing an already-loaded branch
// returns the existing worktree instead of a duplicate; importing a
// second branch adds a second worktree to the SAME canonical.
func TestImport_IdempotentPerBranch(t *testing.T) {
	svc, workRoot := setupRepoFirstImport(t)
	ctx := context.Background()

	first, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main")
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	again, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main")
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if again.WorktreeID != first.WorktreeID {
		t.Errorf("re-import minted a new worktree %s, want existing %s", again.WorktreeID, first.WorktreeID)
	}

	feature, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "feature")
	if err != nil {
		t.Fatalf("feature import: %v", err)
	}
	if feature.WorktreeID == first.WorktreeID {
		t.Error("feature import reused main's worktree")
	}
	if feature.Branch != "feature" {
		t.Errorf("feature branch = %q", feature.Branch)
	}

	// One canonical, two linked worktrees.
	gitDir := filepath.Join(workRoot, "repos", "acme__api", "repo.git")
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

// TestImport_AgentBranchSwitch: branch↔worktree is git's LIVE invariant,
// not a frozen binding — an agent switching branches inside a worktree
// is fully supported and later loads of the now-free branch see reality.
func TestImport_AgentBranchSwitch(t *testing.T) {
	svc, workRoot := setupRepoFirstImport(t)
	ctx := context.Background()

	res, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main")
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// Agent creates + switches to a new branch inside the worktree.
	cmd := exec.Command("git", "switch", "-c", "agent-made")
	cmd.Dir = res.WorktreeDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git switch -c: %v\n%s", err, out)
	}

	gitDir := filepath.Join(workRoot, "repos", "acme__api", "repo.git")
	nowOn, err := git.FindWorktreeForBranch(gitDir, "agent-made")
	if err != nil {
		t.Fatal(err)
	}
	// git prints symlink-resolved paths (macOS /var → /private/var);
	// resolve the expectation before comparing.
	wantDir, err := filepath.EvalSymlinks(res.WorktreeDir)
	if err != nil {
		t.Fatal(err)
	}
	if nowOn == nil || nowOn.Path != wantDir {
		t.Errorf("agent-made not reported at the worktree: %+v, want path %s", nowOn, wantDir)
	}
	freed, err := git.FindWorktreeForBranch(gitDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if freed != nil {
		t.Errorf("main still reported checked out at %+v after the switch", freed)
	}

	// The freed branch is loadable into a fresh worktree.
	reload, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main")
	if err != nil {
		t.Fatalf("reload freed branch: %v", err)
	}
	if reload.WorktreeID == res.WorktreeID {
		t.Error("reload returned the switched-away worktree")
	}
}

// TestDeleteWorktree_LinkedKeepsBranchRef: deleting a repo-first
// worktree goes through `git worktree remove` (bookkeeping released, so
// the branch is re-loadable) while the branch ref and the canonical
// survive.
func TestDeleteWorktree_LinkedKeepsBranchRef(t *testing.T) {
	svc, workRoot := setupRepoFirstImport(t)
	ctx := context.Background()

	res, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "feature")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := svc.DeleteWorktree(ctx, res.WorktreeID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := os.Stat(res.WorktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir still present: err=%v", err)
	}
	gitDir := filepath.Join(workRoot, "repos", "acme__api", "repo.git")
	if _, err := os.Stat(gitDir); err != nil {
		t.Fatalf("canonical removed by worktree delete: %v", err)
	}
	// Branch ref kept…
	exists, err := git.BranchExists(gitDir, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("branch ref deleted with the worktree — want it kept")
	}
	// …and free to load again (bookkeeping was released, not stranded).
	reload, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "feature")
	if err != nil {
		t.Fatalf("reload after delete: %v", err)
	}
	if reload.WorktreeID == res.WorktreeID {
		t.Error("reload returned the deleted worktree id")
	}
}

// TestScaffold_RepoFirstLayout: a greenfield app is a REAL repo from
// birth — bare canonical + linked worktree + one seed commit — and
// origin config written from ANY worktree lands in the shared canonical
// config (the publish flow's core assumption).
func TestScaffold_RepoFirstLayout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workRoot := filepath.Join(t.TempDir(), "work")
	prevRoot := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prevRoot) })

	templateURL, files := makeTemplateRepo(t)
	svc := newTestService(t)

	res, err := svc.CreateProjectFromTemplate(context.Background(), templateURL, "", "My App")
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if res.RepoSlug != "My-App" {
		t.Errorf("slug = %q, want My-App", res.RepoSlug)
	}
	if res.Branch != "main" {
		t.Errorf("branch = %q, want main", res.Branch)
	}
	wantDir := filepath.Join(workRoot, res.WorktreeID)
	if res.WorktreeDir != wantDir {
		t.Errorf("worktree dir = %q, want flat %q", res.WorktreeDir, wantDir)
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(res.WorktreeDir, f)); err != nil {
			t.Errorf("template file %q missing: %v", f, err)
		}
	}

	gitDir := filepath.Join(workRoot, "repos", "My-App", "repo.git")
	label, err := git.GetLocalConfig(gitDir, "clank.repo-label")
	if err != nil || label != "My App" {
		t.Errorf("label = %q (err %v), want My App", label, err)
	}
	// No origin yet — greenfield.
	if _, err := git.RemoteURL(res.WorktreeDir, "origin"); err == nil {
		t.Error("greenfield scaffold has an origin remote — want none until publish")
	}
	// Exactly one seed commit, template history dropped.
	out, err := exec.Command("git", "-C", res.WorktreeDir, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Errorf("seed history = %s commits, want 1", got)
	}
	// Worktree-id stamped (per-worktree, via the shared git dir).
	stamped, err := agent.ReadLocalWorktreeID(res.WorktreeDir)
	if err != nil || stamped != res.WorktreeID {
		t.Errorf("stamped id = %q (err %v), want %q", stamped, err, res.WorktreeID)
	}

	// Publish's shared-config property: RemoteAdd from the WORKTREE is
	// visible from the canonical (and thus every other worktree).
	if err := git.RemoteAdd(res.WorktreeDir, "origin", "https://github.com/me/my-app.git"); err != nil {
		t.Fatalf("RemoteAdd: %v", err)
	}
	fromCanonical, err := git.RemoteURL(gitDir, "origin")
	if err != nil {
		t.Fatalf("RemoteURL from canonical: %v", err)
	}
	if fromCanonical != "https://github.com/me/my-app.git" {
		t.Errorf("canonical origin = %q — RemoteAdd from a worktree must write the shared config", fromCanonical)
	}
}

// TestImport_CleansUpOnAddWorktreeFailure: when a second branch's `git
// worktree add` fails (its target directory can't be created), the
// work root must be left exactly as it was — no orphaned directory, no
// dangling `git worktree` bookkeeping in the canonical — so a retry
// starts clean.
func TestImport_CleansUpOnAddWorktreeFailure(t *testing.T) {
	if _, err := exec.LookPath("chattr"); err != nil {
		t.Skip("chattr not available — can't force a worktree-add failure on this host")
	}
	svc, workRoot := setupRepoFirstImport(t)
	ctx := context.Background()

	first, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "main")
	if err != nil {
		t.Fatalf("first import: %v", err)
	}

	// Lock the work root so the second import's `git worktree add` can't
	// create its target directory — immutable blocks even root.
	if out, err := exec.Command("chattr", "+i", workRoot).CombinedOutput(); err != nil {
		t.Skipf("chattr +i unsupported on this filesystem: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("chattr", "-i", workRoot).Run() })

	if _, err := svc.ImportProjectFromGitHub(ctx, "acme", "api", "feature"); err == nil {
		t.Fatal("expected the second import to fail while the work root is locked")
	}

	if err := exec.Command("chattr", "-i", workRoot).Run(); err != nil {
		t.Fatalf("unlock work root: %v", err)
	}

	entries, err := os.ReadDir(workRoot)
	if err != nil {
		t.Fatalf("ReadDir workRoot: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "repos" && e.Name() != filepath.Base(first.WorktreeDir) {
			t.Errorf("orphaned worktree dir after failed add: %v", e.Name())
		}
	}

	gitDir := filepath.Join(workRoot, "repos", "acme__api", "repo.git")
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
	if linked != 1 {
		t.Errorf("linked worktrees = %d, want 1 (only main's — no orphan for the failed feature add)", linked)
	}
}
