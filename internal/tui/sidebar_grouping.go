package tui

import (
	"github.com/acksell/clank/internal/agent"
)

// MaxRecentSessionsBeforeBucket is how many of the most-recent sessions
// inside an expanded worktree stay visible before the rest collapse
// into a per-worktree "+ show more" bucket. Sessions don't follow the
// week-window worktree rule because a single worktree often accumulates
// many sessions across multiple days; a count cap keeps the list short.
const MaxRecentSessionsBeforeBucket = 5

// RecentWindowDays is the rolling window — anchored on the most-recent
// worktree's activity — within which a worktree stays visible. Wide
// enough that creating a fresh session "today" doesn't suddenly hide
// everything you touched earlier in the week, narrow enough that
// projects untouched for over a week fall away. If you go heads-down
// on a single repo for a week, only that one ends up visible — which
// matches the mental "this is my current focus" model.
const RecentWindowDays = 7

// PartitionWorktreesByActivityWindow keeps every worktree whose latest
// activity sits within RecentWindowDays of the most-recently-active
// worktree. The cwd's worktree is always kept visible regardless of
// age — opening the TUI from a folder is the strongest possible signal
// that you care about whatever sessions live there. Everything else
// moves into the top-level overflow bucket.
//
// The caller is responsible for having sorted the input by latest
// activity descending; the head of the slice anchors the window.
// cwdLocalPath may be empty (no canonical cwd repo resolved); in that
// case only the window filter applies.
func PartitionWorktreesByActivityWindow(worktrees []worktreeNode, cwdLocalPath string) (recent, older []worktreeNode) {
	if len(worktrees) == 0 {
		return nil, nil
	}
	cutoff := worktrees[0].LatestUpdatedAt.AddDate(0, 0, -RecentWindowDays)
	recent = make([]worktreeNode, 0, len(worktrees))
	older = make([]worktreeNode, 0)
	for _, w := range worktrees {
		switch {
		case cwdLocalPath != "" && w.LocalPath == cwdLocalPath:
			recent = append(recent, w)
		case !w.LatestUpdatedAt.Before(cutoff):
			recent = append(recent, w)
		default:
			older = append(older, w)
		}
	}
	return recent, older
}

// PartitionSessionsByRecency keeps the first MaxRecentSessionsBeforeBucket
// sessions visible and folds the rest into a per-worktree overflow
// bucket. Sorted-newest-first input is assumed.
func PartitionSessionsByRecency(sessions []agent.SessionInfo) (recent, older []agent.SessionInfo) {
	if len(sessions) <= MaxRecentSessionsBeforeBucket {
		return append([]agent.SessionInfo(nil), sessions...), nil
	}
	recent = append([]agent.SessionInfo(nil), sessions[:MaxRecentSessionsBeforeBucket]...)
	older = append([]agent.SessionInfo(nil), sessions[MaxRecentSessionsBeforeBucket:]...)
	return recent, older
}
