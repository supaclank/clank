package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRepoAutoTrack_DisabledByDefault: a fresh repo is not auto-tracked.
func TestRepoAutoTrack_DisabledByDefault(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)

	tracked, err := IsRepoAutoTracked(repo)
	if err != nil {
		t.Fatalf("IsRepoAutoTracked: %v", err)
	}
	if tracked {
		t.Error("fresh repo reported auto-tracked")
	}
}

// TestRepoAutoTrack_EnableThenRead: Enable writes the marker under the
// shared .git/clank/ dir and IsRepoAutoTracked then reports true.
func TestRepoAutoTrack_EnableThenRead(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)

	if err := EnableRepoAutoTrack(repo); err != nil {
		t.Fatalf("EnableRepoAutoTrack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "clank", "auto-track")); err != nil {
		t.Errorf(".git/clank/auto-track not created: %v", err)
	}
	tracked, err := IsRepoAutoTracked(repo)
	if err != nil {
		t.Fatalf("IsRepoAutoTracked: %v", err)
	}
	if !tracked {
		t.Error("repo not reported auto-tracked after Enable")
	}
}

// TestRepoAutoTrack_DisableRemovesMarker: Disable clears the marker and
// reports whether one was present (true once, false thereafter).
func TestRepoAutoTrack_DisableRemovesMarker(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)

	if err := EnableRepoAutoTrack(repo); err != nil {
		t.Fatalf("EnableRepoAutoTrack: %v", err)
	}
	removed, err := DisableRepoAutoTrack(repo)
	if err != nil {
		t.Fatalf("DisableRepoAutoTrack: %v", err)
	}
	if !removed {
		t.Error("DisableRepoAutoTrack reported no marker present, want true")
	}
	tracked, err := IsRepoAutoTracked(repo)
	if err != nil {
		t.Fatalf("IsRepoAutoTracked: %v", err)
	}
	if tracked {
		t.Error("repo still auto-tracked after Disable")
	}
	// Second disable is a no-op.
	removed, err = DisableRepoAutoTrack(repo)
	if err != nil {
		t.Fatalf("DisableRepoAutoTrack (2nd): %v", err)
	}
	if removed {
		t.Error("DisableRepoAutoTrack reported a marker on the second call")
	}
}

// TestRepoAutoTrack_VisibleFromLinkedWorktree is the headline property:
// the marker is written to the shared common git dir, so opting the repo
// in from the main worktree auto-tracks every `git worktree add` sibling —
// including ones that don't exist yet at init time.
func TestRepoAutoTrack_VisibleFromLinkedWorktree(t *testing.T) {
	t.Parallel()
	main := initGitRepo(t)
	runGit(t, main, "commit", "--allow-empty", "-m", "init")
	runGit(t, main, "branch", "feature")
	linkedDir := filepath.Join(t.TempDir(), "linked")
	runGit(t, main, "worktree", "add", linkedDir, "feature")

	// Opt in from the main worktree only.
	if err := EnableRepoAutoTrack(main); err != nil {
		t.Fatalf("EnableRepoAutoTrack(main): %v", err)
	}

	tracked, err := IsRepoAutoTracked(linkedDir)
	if err != nil {
		t.Fatalf("IsRepoAutoTracked(linked): %v", err)
	}
	if !tracked {
		t.Error("linked worktree not auto-tracked though the repo is opted in")
	}

	// Disabling from the linked worktree clears it for the whole repo.
	if _, err := DisableRepoAutoTrack(linkedDir); err != nil {
		t.Fatalf("DisableRepoAutoTrack(linked): %v", err)
	}
	tracked, err = IsRepoAutoTracked(main)
	if err != nil {
		t.Fatalf("IsRepoAutoTracked(main) after disable: %v", err)
	}
	if tracked {
		t.Error("main worktree still auto-tracked after disabling from the linked one")
	}
}

// TestRepoAutoTrack_NotAGitRepo: outside a git repo, IsRepoAutoTracked is
// a quiet false (no error) so push/disable don't blow up.
func TestRepoAutoTrack_NotAGitRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	tracked, err := IsRepoAutoTracked(dir)
	if err != nil {
		t.Fatalf("IsRepoAutoTracked: %v", err)
	}
	if tracked {
		t.Error("non-git dir reported auto-tracked")
	}
	removed, err := DisableRepoAutoTrack(dir)
	if err != nil {
		t.Fatalf("DisableRepoAutoTrack: %v", err)
	}
	if removed {
		t.Error("non-git dir reported a marker removed")
	}
}

// TestCommonGitDir_SharedAcrossWorktrees pins the mechanism the marker
// relies on: every worktree resolves to the same common git dir, while
// each has its own per-worktree GitDir.
func TestCommonGitDir_SharedAcrossWorktrees(t *testing.T) {
	t.Parallel()
	main := initGitRepo(t)
	runGit(t, main, "commit", "--allow-empty", "-m", "init")
	runGit(t, main, "branch", "feature")
	linkedDir := filepath.Join(t.TempDir(), "linked")
	runGit(t, main, "worktree", "add", linkedDir, "feature")

	mainCommon, err := CommonGitDir(main)
	if err != nil {
		t.Fatalf("CommonGitDir(main): %v", err)
	}
	linkedCommon, err := CommonGitDir(linkedDir)
	if err != nil {
		t.Fatalf("CommonGitDir(linked): %v", err)
	}
	if mainCommon != linkedCommon {
		t.Errorf("common git dir differs across worktrees: main=%q linked=%q", mainCommon, linkedCommon)
	}

	linkedGitDir, err := GitDir(linkedDir)
	if err != nil {
		t.Fatalf("GitDir(linked): %v", err)
	}
	if linkedGitDir == linkedCommon {
		t.Errorf("linked worktree GitDir (%q) should differ from the common dir (%q)", linkedGitDir, linkedCommon)
	}
}
