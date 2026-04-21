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
// Two optional locator fields:
//
//   - LocalPath: an absolute filesystem path on the *target host*. When
//     present and usable, the host opens this path directly — no clone.
//   - RemoteURL: a git remote URL. When LocalPath is empty or not usable
//     on the target host, the host clones RemoteURL into its clones dir
//     and works from there.
//
// At least one MUST be set. Both is the common case for clients that
// are running co-located with the target host (laptop TUI in $repo): the
// host uses LocalPath and ignores RemoteURL. A client targeting a
// *different* host (e.g. mobile) sends only RemoteURL; the host clones.
// A laptop TUI targeting a different host can safely send both — the
// remote host will fail the LocalPath check and fall through to clone.
//
// Resolution precedence on the host (see host.Service.workDirFor):
//  1. If LocalPath is set, exists, and is the repo root → use it.
//  2. Else if RemoteURL is set → clone into <clonesDir>/<CloneDirName>.
//  3. Else → error.
//
// This is precedence, not a fallback in the AGENTS.md "no fallbacks"
// sense: it's the documented contract every caller relies on. Clients
// that have a LocalPath are expected to send it; mobile clients that
// don't are expected to send only RemoteURL.
type GitRef struct {
	LocalPath      string `json:"local_path,omitempty"`
	RemoteURL      string `json:"remote_url,omitempty"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`
}

// Validate enforces: at least one of LocalPath / RemoteURL is set, and
// each set field is well-formed. Both being set is allowed (and normal
// for laptop clients).
func (g GitRef) Validate() error {
	hasLocal := strings.TrimSpace(g.LocalPath) != ""
	hasRemote := strings.TrimSpace(g.RemoteURL) != ""
	if !hasLocal && !hasRemote {
		return fmt.Errorf("git ref must set at least one of local_path or remote_url")
	}
	if hasLocal && !filepath.IsAbs(g.LocalPath) {
		return fmt.Errorf("local_path must be absolute, got %q", g.LocalPath)
	}
	if hasRemote {
		if _, _, err := parseGitURL(g.RemoteURL); err != nil {
			return fmt.Errorf("remote_url is not a recognized git URL: %w", err)
		}
	}
	return nil
}

// RepoKey returns a stable map key for a GitRef. Used by in-memory dedup
// tables (e.g. the primary-agents background-refresh set) where the
// identity is (project, branch).
//
// Prefers RemoteURL because it is the cross-machine stable identity:
// two clients on different hosts referring to the same project share a
// RemoteURL but have different LocalPaths. Falls back to LocalPath for
// repos with no configured origin. Returns "" for invalid refs.
func RepoKey(g GitRef) string {
	switch {
	case g.RemoteURL != "":
		return "R\x00" + g.RemoteURL + "\x00" + g.WorktreeBranch
	case g.LocalPath != "":
		return "L\x00" + g.LocalPath + "\x00" + g.WorktreeBranch
	default:
		return ""
	}
}

// RepoDisplayName returns a short human-readable label for UIs and logs.
//
// Prefers RemoteURL (parses the project name out of the URL) so the
// display matches across hosts. Falls back to filepath.Base(LocalPath)
// for repos with no configured origin. Returns "" for invalid refs.
func RepoDisplayName(g GitRef) string {
	if g.RemoteURL != "" {
		_, path, err := parseGitURL(g.RemoteURL)
		if err == nil {
			if i := strings.LastIndex(path, "/"); i >= 0 {
				return path[i+1:]
			}
			return path
		}
		// Fall through to LocalPath if the remote URL is malformed.
	}
	if g.LocalPath != "" && filepath.IsAbs(g.LocalPath) {
		return filepath.Base(filepath.Clean(g.LocalPath))
	}
	return ""
}
