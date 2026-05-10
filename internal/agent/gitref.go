package agent

// GitRef is the canonical wire-level identity for a git repository plus
// an optional worktree branch. Lives in the agent package because it is
// embedded in StartRequest; the host package consumes it directly.

import (
	"fmt"
	"path/filepath"
	"strings"
)

// GitRef tells a host where to find a project's files.
//
// Two locator fields, used per-host:
//
//   - LocalPath: an absolute filesystem path on the *target host*.
//     When the target host is co-located with the client (laptop TUI
//     talking to laptop clankd), this is the user's repo path; the
//     host opens it directly.
//   - WorktreeID: the server-assigned worktree ULID minted by
//     clank-sync's `POST /v1/worktrees`. Cached at
//     `<repo>/.clank/worktree-id` after `clank sync push`. When the
//     target host is *not* co-located (docker stack, cloud sandbox),
//     it ignores LocalPath and resolves the worktree from
//     `~/work/<WorktreeID>/`, which the gateway populates during
//     MigrateWorktree.
//
// At least one MUST be set. Both is the common laptop pattern (TUI
// also sends WorktreeID so a future migration to a remote host
// doesn't require re-creating the session).
//
// Resolution precedence on the host (see host.Service.workDirFor):
//  1. If LocalPath is set and points at a valid repo on this host →
//     use it directly.
//  2. Else if WorktreeID is set → use ~/work/<WorktreeID>/. Error if
//     that directory doesn't exist (the gateway must run a migration
//     for this worktree first; we deliberately do not silently clone
//     from origin or pull from the mirror — the model is "synced
//     worktree, single happy path", not "fall back to clone").
//  3. Else → error.
//
// DisplayName is an optional human-readable label set by the
// originating client (typically filepath.Base of LocalPath). UIs and
// logs use it; if empty, callers derive a label from the locator
// fields.
type GitRef struct {
	LocalPath      string `json:"local_path,omitempty"`
	WorktreeID     string `json:"worktree_id,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`
}

// Validate enforces: at least one of LocalPath / WorktreeID is set,
// LocalPath (when set) is absolute. WorktreeID format is not asserted
// here — the host's lookup of ~/work/<WorktreeID>/ catches invalid
// values via the underlying validRepoSlug check.
func (g GitRef) Validate() error {
	hasLocal := strings.TrimSpace(g.LocalPath) != ""
	hasWorktree := strings.TrimSpace(g.WorktreeID) != ""
	if !hasLocal && !hasWorktree {
		return fmt.Errorf("git ref must set at least one of local_path or worktree_id")
	}
	if hasLocal && !filepath.IsAbs(g.LocalPath) {
		return fmt.Errorf("local_path must be absolute, got %q", g.LocalPath)
	}
	return nil
}

// RepoKey returns a stable map key for a GitRef. Used by in-memory
// dedup tables (e.g. the primary-agents background-refresh set) where
// the identity is (project, branch).
//
// Prefers WorktreeID because it is the cross-machine stable identity:
// two clients on different hosts referring to the same project share a
// WorktreeID but have different LocalPaths. Falls back to LocalPath
// for refs with no WorktreeID. Returns "" for invalid refs.
func RepoKey(g GitRef) string {
	switch {
	case g.WorktreeID != "":
		return "W\x00" + g.WorktreeID + "\x00" + g.WorktreeBranch
	case g.LocalPath != "":
		return "L\x00" + g.LocalPath + "\x00" + g.WorktreeBranch
	default:
		return ""
	}
}

// RepoDisplayName returns a short human-readable label for UIs and logs.
//
// Precedence: explicit DisplayName → basename(LocalPath) → "" for
// remote-only refs whose owner did not stamp a DisplayName.
func RepoDisplayName(g GitRef) string {
	if s := strings.TrimSpace(g.DisplayName); s != "" {
		return s
	}
	if g.LocalPath != "" && filepath.IsAbs(g.LocalPath) {
		return filepath.Base(filepath.Clean(g.LocalPath))
	}
	return ""
}
