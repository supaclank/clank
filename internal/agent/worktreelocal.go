package agent

// Cached worktree ULID lives at $(git rev-parse --absolute-git-dir)/clank/worktree-id.
// Inside .git/ so it doesn't pollute the working tree and so each
// `git worktree add` sibling gets its own ID automatically (their
// $gitDir resolves to .git/worktrees/<name>/).

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnvWorktreeID overrides the cached ID resolution. Intended for CI
// and tests that want to pin an ID without touching the repo's .git.
const EnvWorktreeID = "CLANK_WORKTREE_ID"

// worktreeIDRelPath is the path of the cache file relative to gitDir.
const worktreeIDRelPath = "clank/worktree-id"

// errNotGitRepo lets ReadLocalWorktreeID treat "outside a git repo" as
// "no id cached" while still surfacing PATH / permission / IO failures
// from `git rev-parse`.
var errNotGitRepo = errors.New("not a git repository")

// ReadLocalWorktreeID returns the worktree ULID cached for projectDir:
//
//  1. $CLANK_WORKTREE_ID if non-empty.
//  2. <gitDir>/clank/worktree-id (where gitDir = git rev-parse --absolute-git-dir).
//  3. "" if the file is missing or projectDir is not inside a git repo.
//
// Errors other than "not a git repo" / "file missing" propagate so a
// misconfiguration (bad permissions, etc.) doesn't silently degrade
// to "no id cached".
func ReadLocalWorktreeID(projectDir string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(EnvWorktreeID)); v != "" {
		return v, nil
	}
	if projectDir == "" {
		return "", nil
	}
	gd, err := gitDir(projectDir)
	if err != nil {
		if errors.Is(err, errNotGitRepo) {
			return "", nil
		}
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(gd, worktreeIDRelPath))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteLocalWorktreeID persists the worktree ULID for projectDir at
// <gitDir>/clank/worktree-id. Idempotent. Errors if projectDir is not
// inside a git repo (the only caller is `clank push`, which is always
// invoked from a real git working tree).
func WriteLocalWorktreeID(projectDir, id string) error {
	if id == "" {
		return fmt.Errorf("write worktree id: id is empty")
	}
	gd, err := gitDir(projectDir)
	if err != nil {
		return fmt.Errorf("write worktree id: %w", err)
	}
	dir := filepath.Join(gd, "clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "worktree-id"), []byte(id+"\n"), 0o644)
}

// gitDir resolves the per-worktree git directory for projectDir.
// For the main worktree this is <repo>/.git; for a linked worktree
// created by `git worktree add` it's <repo>/.git/worktrees/<name>/.
// Returns an error if projectDir isn't inside a git repo (or git is
// missing from PATH).
func gitDir(projectDir string) (string, error) {
	cmd := exec.Command("git", "-C", projectDir, "rev-parse", "--absolute-git-dir")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(strings.ToLower(string(exitErr.Stderr)), "not a git repository") {
			return "", errNotGitRepo
		}
		return "", fmt.Errorf("git rev-parse --absolute-git-dir in %s: %w", projectDir, err)
	}
	return strings.TrimSpace(string(out)), nil
}
