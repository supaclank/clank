package agent

// LocalWorktreeID is the file under a project root that caches the
// server-assigned worktree ULID. Written by `clank sync push` on first
// registration and read by any caller that needs to identify a project
// to a remote host (TUI session-create, future watcher, etc.).
//
// Path: <repo>/.clank/worktree-id
//
// Kept in this small file rather than internal/clanksync so callers
// in internal/agent and internal/tui don't pull in the broader sync
// substrate just to look up an id.

import (
	"os"
	"path/filepath"
	"strings"
)

// WorktreeIDFile is the path component appended to a project root to
// locate the cached worktree ID.
const WorktreeIDFile = ".clank/worktree-id"

// ReadLocalWorktreeID returns the cached worktree ULID at
// <projectDir>/.clank/worktree-id, or "" if the file is absent or
// empty. Errors other than ErrNotExist propagate so misconfiguration
// (bad permissions, etc.) doesn't silently degrade to "no id".
func ReadLocalWorktreeID(projectDir string) (string, error) {
	if projectDir == "" {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(projectDir, WorktreeIDFile))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteLocalWorktreeID persists the server-assigned worktree ULID at
// <projectDir>/.clank/worktree-id. Idempotent — overwrites any
// existing value. The .clank/ directory is created with 0o755.
func WriteLocalWorktreeID(projectDir, id string) error {
	dir := filepath.Join(projectDir, ".clank")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "worktree-id"), []byte(id+"\n"), 0o644)
}
