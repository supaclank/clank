package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo creates a git repository in a temporary directory with an
// initial commit. Returns the repo path. The repo is cleaned up when the
// test finishes.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Test")
	// Create an initial commit so HEAD and branches exist.
	writeFile(t, filepath.Join(dir, "README.md"), "# test\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "initial commit")
	return dir
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %s\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func TestRepoRoot(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	// From the root itself.
	root, err := RepoRoot(dir)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	// Resolve symlinks for macOS /private/var/folders vs /var/folders.
	wantRoot, _ := filepath.EvalSymlinks(dir)
	gotRoot, _ := filepath.EvalSymlinks(root)
	if gotRoot != wantRoot {
		t.Errorf("RepoRoot = %q, want %q", gotRoot, wantRoot)
	}

	// From a subdirectory.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	root, err = RepoRoot(sub)
	if err != nil {
		t.Fatalf("RepoRoot(sub): %v", err)
	}
	gotRoot, _ = filepath.EvalSymlinks(root)
	if gotRoot != wantRoot {
		t.Errorf("RepoRoot(sub) = %q, want %q", gotRoot, wantRoot)
	}
}

func TestRepoRootNotARepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // not a git repo
	_, err := RepoRoot(dir)
	if err == nil {
		t.Fatal("expected error for non-git directory")
	}
}

func TestCurrentBranch(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	branch, err := CurrentBranch(dir)
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	// Git default branch could be main or master depending on config.
	if branch != "main" && branch != "master" {
		t.Errorf("CurrentBranch = %q, want main or master", branch)
	}

	// Create and switch to a new branch.
	run(t, dir, "git", "checkout", "-b", "feat/test-branch")
	branch, err = CurrentBranch(dir)
	if err != nil {
		t.Fatalf("CurrentBranch after checkout: %v", err)
	}
	if branch != "feat/test-branch" {
		t.Errorf("CurrentBranch = %q, want feat/test-branch", branch)
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	branch, err := DefaultBranch(dir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	// The test repo init uses whatever git's default is (main or master).
	if branch != "main" && branch != "master" {
		t.Errorf("DefaultBranch = %q, want main or master", branch)
	}
}

func TestLocalBranches(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	// Create a couple of branches.
	run(t, dir, "git", "branch", "feat/login")
	run(t, dir, "git", "branch", "fix/auth")

	branches, err := LocalBranches(dir)
	if err != nil {
		t.Fatalf("LocalBranches: %v", err)
	}

	// Should have at least 3 branches (default + feat/login + fix/auth).
	if len(branches) < 3 {
		t.Fatalf("expected at least 3 branches, got %d: %v", len(branches), branches)
	}

	found := make(map[string]bool)
	for _, b := range branches {
		found[b] = true
	}
	if !found["feat/login"] {
		t.Error("missing feat/login branch")
	}
	if !found["fix/auth"] {
		t.Error("missing fix/auth branch")
	}
}

func TestBranchExists(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	run(t, dir, "git", "branch", "feat/exists")

	exists, err := BranchExists(dir, "feat/exists")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if !exists {
		t.Error("expected feat/exists to exist")
	}

	exists, err = BranchExists(dir, "feat/does-not-exist")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if exists {
		t.Error("expected feat/does-not-exist to not exist")
	}
}

func TestListWorktrees(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	worktrees, err := ListWorktrees(dir)
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}

	// A fresh repo has one worktree (the main working tree).
	if len(worktrees) != 1 {
		t.Fatalf("expected 1 worktree, got %d: %+v", len(worktrees), worktrees)
	}

	wantPath, _ := filepath.EvalSymlinks(dir)
	gotPath, _ := filepath.EvalSymlinks(worktrees[0].Path)
	if gotPath != wantPath {
		t.Errorf("worktree path = %q, want %q", gotPath, wantPath)
	}
	if worktrees[0].Branch == "" {
		t.Error("expected worktree to have a branch")
	}
}

func TestAddWorktreeExistingBranch(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	// Create a branch to check out in the worktree.
	run(t, dir, "git", "branch", "feat/worktree-test")

	wtDir := filepath.Join(t.TempDir(), "wt")
	err := AddWorktree(dir, wtDir, "feat/worktree-test")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	// Verify the worktree was created.
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Fatal("worktree directory not created")
	}

	// Verify it appears in the worktree list.
	worktrees, err := ListWorktrees(dir)
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	if len(worktrees) != 2 {
		t.Fatalf("expected 2 worktrees, got %d", len(worktrees))
	}

	// Verify the branch is correct.
	found := false
	for _, wt := range worktrees {
		if wt.Branch == "feat/worktree-test" {
			found = true
			break
		}
	}
	if !found {
		t.Error("worktree for feat/worktree-test not found in list")
	}
}

func TestAddWorktreeNewBranch(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	defaultBranch, err := DefaultBranch(dir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	wtDir := filepath.Join(t.TempDir(), "wt-new")
	err = AddWorktreeNewBranch(dir, wtDir, "feat/brand-new", defaultBranch)
	if err != nil {
		t.Fatalf("AddWorktreeNewBranch: %v", err)
	}

	// Verify branch was created.
	exists, err := BranchExists(dir, "feat/brand-new")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if !exists {
		t.Error("expected feat/brand-new branch to be created")
	}

	// Verify the worktree is on that branch.
	branch, err := CurrentBranch(wtDir)
	if err != nil {
		t.Fatalf("CurrentBranch(wtDir): %v", err)
	}
	if branch != "feat/brand-new" {
		t.Errorf("worktree branch = %q, want feat/brand-new", branch)
	}
}

func TestFindWorktreeForBranch(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	run(t, dir, "git", "branch", "feat/find-me")
	wtDir := filepath.Join(t.TempDir(), "wt-find")
	if err := AddWorktree(dir, wtDir, "feat/find-me"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	wt, err := FindWorktreeForBranch(dir, "feat/find-me")
	if err != nil {
		t.Fatalf("FindWorktreeForBranch: %v", err)
	}
	if wt == nil {
		t.Fatal("expected to find worktree for feat/find-me")
	}
	if wt.Branch != "feat/find-me" {
		t.Errorf("worktree branch = %q, want feat/find-me", wt.Branch)
	}

	// Non-existent branch should return nil.
	wt, err = FindWorktreeForBranch(dir, "feat/does-not-exist")
	if err != nil {
		t.Fatalf("FindWorktreeForBranch: %v", err)
	}
	if wt != nil {
		t.Error("expected nil for non-existent branch worktree")
	}
}

func TestRemoveWorktree(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	run(t, dir, "git", "branch", "feat/remove-me")
	wtDir := filepath.Join(t.TempDir(), "wt-rm")
	if err := AddWorktree(dir, wtDir, "feat/remove-me"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}

	// Remove it.
	err := RemoveWorktree(dir, wtDir, false)
	if err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}

	// Should be gone from the list.
	worktrees, err := ListWorktrees(dir)
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	for _, wt := range worktrees {
		if wt.Branch == "feat/remove-me" {
			t.Error("worktree for feat/remove-me still exists after removal")
		}
	}
}

func TestWorktreeDir(t *testing.T) {
	t.Parallel()

	dir, err := WorktreeDir("myproject", "feat/login")
	if err != nil {
		t.Fatalf("WorktreeDir: %v", err)
	}

	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".clank", "worktrees", "myproject", "feat-login")
	if dir != want {
		t.Errorf("WorktreeDir = %q, want %q", dir, want)
	}
}

func TestSanitizeBranchName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"main", "main"},
		{"feat/login", "feat-login"},
		{"fix/auth/bug", "fix-auth-bug"},
		{"my branch", "my-branch"},
	}
	for _, tt := range tests {
		got := sanitizeBranchName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeBranchName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseWorktreeList(t *testing.T) {
	t.Parallel()

	input := `worktree /home/user/project
HEAD abc123def456
branch refs/heads/main

worktree /home/user/project-feat
HEAD 789abc012def
branch refs/heads/feat/login

`
	worktrees := parseWorktreeList(input)
	if len(worktrees) != 2 {
		t.Fatalf("expected 2 worktrees, got %d", len(worktrees))
	}

	if worktrees[0].Path != "/home/user/project" {
		t.Errorf("worktree[0].Path = %q", worktrees[0].Path)
	}
	if worktrees[0].Branch != "main" {
		t.Errorf("worktree[0].Branch = %q", worktrees[0].Branch)
	}
	if worktrees[0].Head != "abc123def456" {
		t.Errorf("worktree[0].Head = %q", worktrees[0].Head)
	}

	if worktrees[1].Path != "/home/user/project-feat" {
		t.Errorf("worktree[1].Path = %q", worktrees[1].Path)
	}
	if worktrees[1].Branch != "feat/login" {
		t.Errorf("worktree[1].Branch = %q", worktrees[1].Branch)
	}
}

func TestIsClean(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	// A fresh repo with no changes should be clean.
	clean, err := IsClean(dir)
	if err != nil {
		t.Fatalf("IsClean: %v", err)
	}
	if !clean {
		t.Error("expected clean repo after init")
	}

	// Untracked files should NOT make the repo dirty — IsClean only
	// checks tracked files so that untracked build artifacts, docs, etc.
	// don't block merges.
	writeFile(t, filepath.Join(dir, "untracked.txt"), "untracked\n")
	clean, err = IsClean(dir)
	if err != nil {
		t.Fatalf("IsClean: %v", err)
	}
	if !clean {
		t.Error("expected clean repo with only untracked files")
	}

	// A modified tracked file should be dirty.
	writeFile(t, filepath.Join(dir, "README.md"), "modified\n")
	clean, err = IsClean(dir)
	if err != nil {
		t.Fatalf("IsClean: %v", err)
	}
	if clean {
		t.Error("expected dirty repo with modified tracked file")
	}

	// Stage the change — still dirty.
	run(t, dir, "git", "add", "README.md")
	clean, err = IsClean(dir)
	if err != nil {
		t.Fatalf("IsClean: %v", err)
	}
	if clean {
		t.Error("expected dirty repo with staged file")
	}

	// Commit it — clean again.
	run(t, dir, "git", "commit", "-m", "update README")
	clean, err = IsClean(dir)
	if err != nil {
		t.Fatalf("IsClean: %v", err)
	}
	if !clean {
		t.Error("expected clean repo after commit")
	}
}

func TestCommitsAhead(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	defaultBranch, err := DefaultBranch(dir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	// Create a feature branch with 3 commits ahead of main.
	run(t, dir, "git", "checkout", "-b", "feat/ahead-test")
	for i := 0; i < 3; i++ {
		writeFile(t, filepath.Join(dir, "file"+string(rune('a'+i))+".txt"), "content\n")
		run(t, dir, "git", "add", ".")
		run(t, dir, "git", "commit", "-m", "commit "+string(rune('a'+i)))
	}

	n, err := CommitsAhead(dir, defaultBranch, "feat/ahead-test")
	if err != nil {
		t.Fatalf("CommitsAhead: %v", err)
	}
	if n != 3 {
		t.Errorf("CommitsAhead = %d, want 3", n)
	}

	// Default branch should be 0 ahead of itself.
	n, err = CommitsAhead(dir, defaultBranch, defaultBranch)
	if err != nil {
		t.Fatalf("CommitsAhead: %v", err)
	}
	if n != 0 {
		t.Errorf("CommitsAhead(main, main) = %d, want 0", n)
	}
}

func TestCommitLog(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	defaultBranch, err := DefaultBranch(dir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	run(t, dir, "git", "checkout", "-b", "feat/log-test")
	writeFile(t, filepath.Join(dir, "log-a.txt"), "a\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "add log-a")
	writeFile(t, filepath.Join(dir, "log-b.txt"), "b\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "add log-b")

	log, err := CommitLog(dir, defaultBranch, "feat/log-test")
	if err != nil {
		t.Fatalf("CommitLog: %v", err)
	}

	lines := strings.Split(log, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %q", len(lines), log)
	}
	// Newest first.
	if !strings.Contains(lines[0], "add log-b") {
		t.Errorf("line[0] = %q, expected to contain 'add log-b'", lines[0])
	}
	if !strings.Contains(lines[1], "add log-a") {
		t.Errorf("line[1] = %q, expected to contain 'add log-a'", lines[1])
	}
}

func TestCheckout(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	run(t, dir, "git", "branch", "feat/checkout-test")
	if err := Checkout(dir, "feat/checkout-test"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	branch, err := CurrentBranch(dir)
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "feat/checkout-test" {
		t.Errorf("CurrentBranch = %q, want feat/checkout-test", branch)
	}
}

func TestMergeNoFF_HappyPath(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	defaultBranch, err := DefaultBranch(dir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	// Create a feature branch with a commit.
	run(t, dir, "git", "checkout", "-b", "feat/merge-test")
	writeFile(t, filepath.Join(dir, "feature.txt"), "feature\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "add feature")

	// Switch back to default branch and merge.
	if err := Checkout(dir, defaultBranch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	err = MergeNoFF(dir, "feat/merge-test", "Merge branch 'feat/merge-test'")
	if err != nil {
		t.Fatalf("MergeNoFF: %v", err)
	}

	// Verify the feature file exists on the default branch.
	if _, statErr := os.Stat(filepath.Join(dir, "feature.txt")); os.IsNotExist(statErr) {
		t.Error("feature.txt not present after merge")
	}

	// Verify a merge commit was created (not fast-forward).
	// A merge commit has 2+ parents.
	out := run(t, dir, "git", "cat-file", "-p", "HEAD")
	parentCount := strings.Count(out, "parent ")
	if parentCount < 2 {
		t.Errorf("expected merge commit with 2+ parents, got %d", parentCount)
	}
}

func TestMergeNoFF_Conflict(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	defaultBranch, err := DefaultBranch(dir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	// Create conflicting changes on a branch and default.
	run(t, dir, "git", "checkout", "-b", "feat/conflict-test")
	writeFile(t, filepath.Join(dir, "README.md"), "branch version\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "branch change")

	if err := Checkout(dir, defaultBranch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	writeFile(t, filepath.Join(dir, "README.md"), "main version\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "main change")

	// Attempt merge — should fail due to conflict.
	err = MergeNoFF(dir, "feat/conflict-test", "Merge conflict branch")
	if err == nil {
		t.Fatal("expected merge conflict error, got nil")
	}

	// Should be in merging state.
	if !IsMerging(dir) {
		t.Error("expected IsMerging to be true after conflict")
	}

	// Abort the merge.
	if err := AbortMerge(dir); err != nil {
		t.Fatalf("AbortMerge: %v", err)
	}

	// Should no longer be merging.
	if IsMerging(dir) {
		t.Error("expected IsMerging to be false after abort")
	}
}

func TestDeleteBranch(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	defaultBranch, err := DefaultBranch(dir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	// Create and merge a branch so it can be safely deleted with -d.
	run(t, dir, "git", "checkout", "-b", "feat/delete-test")
	writeFile(t, filepath.Join(dir, "delete.txt"), "delete me\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "add delete.txt")
	if err := Checkout(dir, defaultBranch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	// Merge so the branch is fully merged (allows -d).
	if err := MergeNoFF(dir, "feat/delete-test", "merge for delete test"); err != nil {
		t.Fatalf("MergeNoFF: %v", err)
	}

	// Delete with safe flag.
	if err := DeleteBranch(dir, "feat/delete-test", false); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}

	exists, err := BranchExists(dir, "feat/delete-test")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if exists {
		t.Error("expected branch to be deleted")
	}
}

func TestDeleteBranch_ForceUnmerged(t *testing.T) {
	t.Parallel()
	dir := initTestRepo(t)

	// Create an unmerged branch.
	run(t, dir, "git", "checkout", "-b", "feat/force-delete")
	writeFile(t, filepath.Join(dir, "unmerged.txt"), "unmerged\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "unmerged commit")

	defaultBranch, err := DefaultBranch(dir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if err := Checkout(dir, defaultBranch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	// Safe delete should fail (unmerged).
	if err := DeleteBranch(dir, "feat/force-delete", false); err == nil {
		t.Error("expected error deleting unmerged branch with safe flag")
	}

	// Force delete should succeed.
	if err := DeleteBranch(dir, "feat/force-delete", true); err != nil {
		t.Fatalf("DeleteBranch -D: %v", err)
	}

	exists, err := BranchExists(dir, "feat/force-delete")
	if err != nil {
		t.Fatalf("BranchExists: %v", err)
	}
	if exists {
		t.Error("expected branch to be force-deleted")
	}
}
