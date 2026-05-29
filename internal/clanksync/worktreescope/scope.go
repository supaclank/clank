// Package worktreescope enumerates a repository's git worktrees with the
// metadata `clank init`/`clank push` need: the cached clank worktree-id
// and a recency signal for filtering to actively-used worktrees.
package worktreescope

import (
	"os"
	"path/filepath"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
)

// DefaultRecencyWindow is how recently a worktree must have been touched
// to count as "active" for autopush enrollment.
const DefaultRecencyWindow = 48 * time.Hour

// Scope is one git worktree plus the clank metadata for autopush.
type Scope struct {
	Path             string
	Branch           string
	WorktreeID       string // cached clank id; empty if not yet registered
	LastActive       time.Time
	IsRecentlyActive bool
}

// WorktreesForRepo lists the git worktrees of the repo containing dir,
// annotated with each one's cached clank worktree-id and recency
// relative to window. The bare entry is skipped.
func WorktreesForRepo(dir string, window time.Duration) ([]Scope, error) {
	wts, err := git.ListWorktrees(dir)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-window)
	out := make([]Scope, 0, len(wts))
	for _, wt := range wts {
		if wt.Bare {
			continue
		}
		id, _ := agent.ReadLocalWorktreeID(wt.Path)
		active := worktreeActivity(wt.Path)
		out = append(out, Scope{
			Path:             wt.Path,
			Branch:           wt.Branch,
			WorktreeID:       id,
			LastActive:       active,
			IsRecentlyActive: !active.IsZero() && !active.Before(cutoff),
		})
	}
	return out, nil
}

// worktreeActivity is a coarse "recently worked in this worktree"
// signal: the mtime of the git index (bumped by add/commit/checkout/
// status), falling back to the worktree directory's mtime. Zero time on
// failure.
func worktreeActivity(path string) time.Time {
	if gd, err := agent.GitDir(path); err == nil {
		if t := statModTime(filepath.Join(gd, "index")); !t.IsZero() {
			return t
		}
	}
	return statModTime(path)
}

func statModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
