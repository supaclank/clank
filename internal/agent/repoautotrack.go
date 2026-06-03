package agent

// Repo-wide auto-track marker lives at
// $(git rev-parse --git-common-dir)/clank/auto-track. Unlike the
// per-worktree id (under --absolute-git-dir), the common dir is shared by
// every worktree of a repo — so the marker opts the WHOLE repo, including
// worktrees added with `git worktree add` later, into lazy registration on
// first `clank push`. Written by `clank init`, honored by ensureTracked.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// repoAutoTrackRelPath is the marker path relative to the common git dir.
const repoAutoTrackRelPath = "clank/auto-track"

// IsRepoAutoTracked reports whether projectDir's repo has been opted into
// repo-wide auto-push (via `clank init`). Returns false (not an error)
// when the marker is absent or projectDir isn't inside a git repo, so
// callers can treat it as a plain "may I auto-register?" predicate.
func IsRepoAutoTracked(projectDir string) (bool, error) {
	cd, err := CommonGitDir(projectDir)
	if err != nil {
		if errors.Is(err, errNotGitRepo) {
			return false, nil
		}
		return false, err
	}
	_, err = os.Stat(filepath.Join(cd, repoAutoTrackRelPath))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// EnableRepoAutoTrack opts projectDir's repo into repo-wide auto-push by
// writing the shared marker. Idempotent. Errors if projectDir isn't inside
// a git repo (the only caller is `clank init`, always run in a real repo).
func EnableRepoAutoTrack(projectDir string) error {
	cd, err := CommonGitDir(projectDir)
	if err != nil {
		return fmt.Errorf("enable repo auto-track: %w", err)
	}
	dir := filepath.Join(cd, "clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cd, repoAutoTrackRelPath), []byte("1\n"), 0o644)
}

// DisableRepoAutoTrack removes projectDir's repo-wide auto-push marker.
// Returns true if the marker was present. Missing marker or non-repo is a
// no-op (false, nil).
func DisableRepoAutoTrack(projectDir string) (bool, error) {
	cd, err := CommonGitDir(projectDir)
	if err != nil {
		if errors.Is(err, errNotGitRepo) {
			return false, nil
		}
		return false, err
	}
	err = os.Remove(filepath.Join(cd, repoAutoTrackRelPath))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// CommonGitDir resolves the shared git directory for projectDir's repo —
// the same absolute path for every worktree of the repo. For the main
// worktree this equals GitDir; for a linked worktree it points back at the
// parent's .git (not .git/worktrees/<name>), so repo-scoped state written
// from one worktree is visible from all of them. Returns errNotGitRepo
// (wrapped) when projectDir isn't inside a git repo.
func CommonGitDir(projectDir string) (string, error) {
	cmd := exec.Command("git", "-C", projectDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(strings.ToLower(string(exitErr.Stderr)), "not a git repository") {
			return "", errNotGitRepo
		}
		return "", fmt.Errorf("git rev-parse --git-common-dir in %s: %w", projectDir, err)
	}
	return strings.TrimSpace(string(out)), nil
}
