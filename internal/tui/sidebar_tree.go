package tui

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/supaclank/clank/internal/agent"
)

// sidebarTree is the conceptual tree the new IDE-style sidebar renders.
// Flatten walks this structure with the expand-state map to produce the
// linear node list the cursor moves over. The struct stores the data
// itself rather than a pre-flattened list so the same tree can be
// re-flattened cheaply as expand state changes.
type sidebarTree struct {
	Home            homeNode
	RecentWorktrees []worktreeNode
	OlderWorktrees  olderWorktreesNode
	Import          importNode
	Cloud           cloudNode
	Settings        settingsNode
}

// buildSidebarTree groups sessions by GitRef.LocalPath, builds a
// worktreeNode for each unique path, sorts the result by latest activity
// descending, and keeps the worktrees whose latest activity falls
// within RecentWindowDays of the most-recently-active worktree visible
// — plus the cwd's worktree, always, so opening the TUI from a stale
// folder still surfaces what lives there. The rest fall into the
// OlderWorktrees bucket. Sessions on a worktree are kept sorted
// newest-first so rendering emits them in activity order without a
// re-sort.
//
// cwdLocalPath is the canonical path of the worktree the TUI was
// launched from (resolveLocalRepo's GitRef.LocalPath). Empty means
// no canonical cwd was resolvable; the always-visible cwd rule is
// then a no-op.
//
// now is currently unused but stays in the signature so callers can
// keep passing an injected clock; future bucketing logic may want it
// without another API churn.
func buildSidebarTree(sessions []agent.SessionInfo, cwdLocalPath string, now time.Time) sidebarTree {
	_ = now
	byPath := groupSessionsByPath(sessions)

	worktrees := make([]worktreeNode, 0, len(byPath))
	for path, infos := range byPath {
		worktrees = append(worktrees, buildWorktreeNode(path, infos))
	}
	sort.Slice(worktrees, func(i, j int) bool {
		return worktrees[i].LatestUpdatedAt.After(worktrees[j].LatestUpdatedAt)
	})

	recent, older := PartitionWorktreesByActivityWindow(worktrees, cwdLocalPath)
	return sidebarTree{
		Home:            homeNode{},
		RecentWorktrees: recent,
		OlderWorktrees:  olderWorktreesNode{Hidden: older},
		Import:          importNode{},
		Cloud:           cloudNode{},
		Settings:        settingsNode{},
	}
}

// groupSessionsByPath buckets sessions by GitRef.LocalPath, dropping
// any session without a LocalPath (those have no worktree to belong to).
func groupSessionsByPath(sessions []agent.SessionInfo) map[string][]agent.SessionInfo {
	out := make(map[string][]agent.SessionInfo)
	for _, s := range sessions {
		path := s.GitRef.LocalPath
		if path == "" {
			continue
		}
		out[path] = append(out[path], s)
	}
	return out
}

// buildWorktreeNode collapses a slice of sessions on a single LocalPath
// into one worktreeNode, computing counts, latest activity, and capturing
// the worktree id off the first session that has one. Sessions are
// sorted newest-first before being attached so the eventual session
// children render in activity order with no additional sort step.
func buildWorktreeNode(path string, sessions []agent.SessionInfo) worktreeNode {
	n := worktreeNode{
		LocalPath: path,
		Label:     filepath.Base(path),
		RepoLabel: deriveRepoLabel(path),
	}
	for _, s := range sessions {
		if n.WorktreeID == "" && s.GitRef.WorktreeID != "" {
			n.WorktreeID = s.GitRef.WorktreeID
		}
		n.Total++
		switch s.Visibility {
		case agent.VisibilityArchived:
			n.Archived++
		case agent.VisibilityDone:
			n.Done++
		default:
			n.Active++
		}
		if s.UpdatedAt.After(n.LatestUpdatedAt) {
			n.LatestUpdatedAt = s.UpdatedAt
		}
	}

	sorted := make([]agent.SessionInfo, len(sessions))
	copy(sorted, sessions)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].UpdatedAt.After(sorted[j].UpdatedAt)
	})
	n.Sessions = sorted
	return n
}

// worktreeMarker is the path segment clank inserts when materializing a
// worktree under its repo. The directory before this marker is the
// owning repo's checkout, which we use as the repo label.
const worktreeMarker = "/.claude/worktrees/"

// deriveRepoLabel returns the repo tag shown next to a worktree row.
// Clank lays worktrees out as <repo>/.claude/worktrees/<name>, so when
// that marker appears in the path the segment immediately before it
// names the owning repo. Other layouts (main checkout, ad-hoc paths)
// return an empty label and render no tag — surfacing a misleading
// directory name would be worse than nothing.
func deriveRepoLabel(path string) string {
	if i := strings.Index(path, worktreeMarker); i >= 0 {
		return filepath.Base(path[:i])
	}
	return ""
}
