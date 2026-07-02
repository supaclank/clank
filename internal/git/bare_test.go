package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeBareTestOrigin builds a local origin with two branches (main +
// feature) and partial-clone support enabled, returning its file:// URL.
// file:// (not a bare path) forces the pack protocol, which is what makes
// --filter honest — a plain-path clone silently ignores filters.
func makeBareTestOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	// Partial clone needs the server side to allow filters.
	run("git", "config", "uploadpack.allowFilter", "true")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "one")
	run("git", "branch", "feature")
	return "file://" + dir
}

func TestCloneBare_BloblessSingleBranchWithConfigs(t *testing.T) {
	t.Parallel()
	origin := makeBareTestOrigin(t)
	gitDir := filepath.Join(t.TempDir(), "repo.git")

	helper := `!"/opt/clank-host" git-credential`
	if err := CloneBare(context.Background(), origin, gitDir, "tok_secret", "", helper); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}

	// Blobless promisor config present.
	for key, want := range map[string]string{
		"remote.origin.promisor":           "true",
		"remote.origin.partialclonefilter": "blob:none",
		"remote.origin.fetch":              allHeadsRefspec,
		"credential.helper":                helper,
		"remote.origin.url":                origin,
	} {
		got, err := GetLocalConfig(gitDir, key)
		if err != nil {
			t.Fatalf("GetLocalConfig(%s): %v", key, err)
		}
		if got != want {
			t.Errorf("config %s = %q, want %q", key, got, want)
		}
	}

	// Single-branch: only main's local ref came over.
	branches, err := LocalBranches(gitDir)
	if err != nil {
		t.Fatalf("LocalBranches: %v", err)
	}
	if len(branches) != 1 || branches[0] != "main" {
		t.Errorf("local branches = %v, want [main]", branches)
	}

	// The token must never land in the repo config.
	cfg, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "tok_secret") {
		t.Error("token leaked into canonical config")
	}

	// A worktree add from the bare blobless canonical materializes files
	// (lazy blob fetch against the file:// promisor — no auth needed).
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(gitDir, wt, "main"); err != nil {
		t.Fatalf("AddWorktree from blobless canonical: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "a.txt")); err != nil {
		t.Errorf("checkout missing file: %v", err)
	}
}

func TestCloneBare_SelectsBranch(t *testing.T) {
	t.Parallel()
	origin := makeBareTestOrigin(t)
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	if err := CloneBare(context.Background(), origin, gitDir, "", "feature", ""); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	head, err := HeadBranch(gitDir)
	if err != nil {
		t.Fatalf("HeadBranch: %v", err)
	}
	if head != "feature" {
		t.Errorf("HEAD = %q, want feature", head)
	}
}

func TestInitBareAndHeadBranch(t *testing.T) {
	t.Parallel()
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	if err := InitBare(context.Background(), gitDir, "trunk"); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	head, err := HeadBranch(gitDir)
	if err != nil {
		t.Fatalf("HeadBranch: %v", err)
	}
	if head != "trunk" {
		t.Errorf("HEAD = %q, want trunk", head)
	}
}

func TestLocalBranchTips(t *testing.T) {
	t.Parallel()
	origin := makeBareTestOrigin(t)
	dir := strings.TrimPrefix(origin, "file://")
	tips, err := LocalBranchTips(dir)
	if err != nil {
		t.Fatalf("LocalBranchTips: %v", err)
	}
	if len(tips) != 2 {
		t.Fatalf("len(tips) = %d, want 2 (main + feature)", len(tips))
	}
	for _, tip := range tips {
		if tip.SHA == "" || tip.CommittedAt.IsZero() {
			t.Errorf("tip %+v missing SHA or CommittedAt", tip)
		}
	}
}

func TestRemoteTrackingBranchExists(t *testing.T) {
	t.Parallel()
	origin := makeBareTestOrigin(t)
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	if err := CloneBare(context.Background(), origin, gitDir, "", "", ""); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	// Fetch all heads so origin/feature exists as a tracking ref.
	if err := Fetch(gitDir, "origin", allHeadsRefspec, PushOptions{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	ok, err := RemoteTrackingBranchExists(gitDir, "origin", "feature")
	if err != nil {
		t.Fatalf("RemoteTrackingBranchExists: %v", err)
	}
	if !ok {
		t.Error("origin/feature missing after all-heads fetch")
	}
	ok, err = RemoteTrackingBranchExists(gitDir, "origin", "nope")
	if err != nil {
		t.Fatalf("RemoteTrackingBranchExists(nope): %v", err)
	}
	if ok {
		t.Error("origin/nope reported present")
	}
}

func TestGetLocalConfig_UnsetKey(t *testing.T) {
	t.Parallel()
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	if err := InitBare(context.Background(), gitDir, "main"); err != nil {
		t.Fatal(err)
	}
	got, err := GetLocalConfig(gitDir, "clank.does-not-exist")
	if err != nil {
		t.Fatalf("GetLocalConfig unset: %v", err)
	}
	if got != "" {
		t.Errorf("unset key = %q, want empty", got)
	}
}

func TestPruneWorktrees_DropsStaleBookkeeping(t *testing.T) {
	t.Parallel()
	origin := makeBareTestOrigin(t)
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	if err := CloneBare(context.Background(), origin, gitDir, "", "", ""); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(gitDir, wt, "main"); err != nil {
		t.Fatal(err)
	}
	// Simulate a manual rm of the worktree dir, then prune.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := PruneWorktrees(gitDir); err != nil {
		t.Fatalf("PruneWorktrees: %v", err)
	}
	wts, err := ListWorktrees(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range wts {
		if w.Path == wt {
			t.Errorf("stale worktree entry survived prune: %+v", w)
		}
	}
}

// TestMainWorktreeRoot_Bare: for a bare canonical the "main worktree
// root" IS the bare dir — not its parent (which isn't a repo at all).
// Guards the still-live CreateWorktree/resolveWorktree paths when their
// base resolves into a repo-first linked worktree.
func TestMainWorktreeRoot_Bare(t *testing.T) {
	t.Parallel()
	origin := makeBareTestOrigin(t)
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	if err := CloneBare(context.Background(), origin, gitDir, "", "", ""); err != nil {
		t.Fatal(err)
	}
	// git reports symlink-resolved absolute paths (macOS /var →
	// /private/var), so compare against the resolved expectation.
	wantDir := mustEvalSymlinks(t, gitDir)

	got, err := MainWorktreeRoot(gitDir)
	if err != nil {
		t.Fatalf("MainWorktreeRoot(bare): %v", err)
	}
	if got != wantDir {
		t.Errorf("MainWorktreeRoot(bare) = %q, want the bare dir %q", got, wantDir)
	}

	// From a linked worktree of the bare canonical, same answer.
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(gitDir, wt, "main"); err != nil {
		t.Fatal(err)
	}
	got, err = MainWorktreeRoot(wt)
	if err != nil {
		t.Fatalf("MainWorktreeRoot(linked): %v", err)
	}
	if got != wantDir {
		t.Errorf("MainWorktreeRoot(linked) = %q, want %q", got, wantDir)
	}
}

// mustEvalSymlinks resolves path's symlinks (macOS tempdirs live under
// the /var → /private/var alias while git prints resolved paths).
func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

func TestCommonDir(t *testing.T) {
	t.Parallel()
	origin := makeBareTestOrigin(t)
	nonBare := strings.TrimPrefix(origin, "file://")

	common, err := CommonDir(nonBare)
	if err != nil {
		t.Fatalf("CommonDir(non-bare): %v", err)
	}
	if filepath.Base(common) != ".git" {
		t.Errorf("CommonDir(non-bare) = %q, want a .git dir", common)
	}

	gitDir := filepath.Join(t.TempDir(), "repo.git")
	if err := CloneBare(context.Background(), origin, gitDir, "", "", ""); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(gitDir, wt, "main"); err != nil {
		t.Fatal(err)
	}
	common, err = CommonDir(wt)
	if err != nil {
		t.Fatalf("CommonDir(linked): %v", err)
	}
	if want := mustEvalSymlinks(t, gitDir); common != want {
		t.Errorf("CommonDir(linked) = %q, want the canonical %q", common, want)
	}
}
