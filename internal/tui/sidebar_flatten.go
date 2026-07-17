package tui

import "time"

// flattenSidebar walks the conceptual tree in display order and emits
// the visible node list given the current expand-state map. The result
// is the single source of truth for cursor math: index i in the slice
// corresponds to the i-th selectable row on screen.
//
// expanded[key]==true means the row identified by key is expanded.
// Missing/false keys are collapsed. Older buckets are intentionally not
// special-cased here — the caller is responsible for resetting their
// keys to collapsed at startup (so they never carry forward across
// restarts).
//
// now is unused today; it stays in the signature so callers can keep
// injecting a deterministic clock for future age-aware behavior
// without another touch to every call site.
func flattenSidebar(t sidebarTree, expanded map[string]bool, now time.Time) []sidebarNode {
	out := make([]sidebarNode, 0, 4+len(t.RecentWorktrees)+len(t.OlderWorktrees.Hidden))
	out = appendWorktreeRows(out, t.RecentWorktrees, expanded, now)
	if len(t.OlderWorktrees.Hidden) > 0 {
		out = append(out, t.OlderWorktrees)
		if expanded[t.OlderWorktrees.Key()] {
			out = appendWorktreeRows(out, t.OlderWorktrees.Hidden, expanded, now)
		}
	}
	out = append(out, t.Import, t.Cloud, t.Settings)
	return out
}

// appendWorktreeRows emits each worktree as a row, and when expanded,
// its session children (split into recent + an older bucket). The older
// bucket itself is only emitted when there are sessions to hide behind
// it — empty buckets would be a dead row the user can't avoid.
func appendWorktreeRows(out []sidebarNode, worktrees []worktreeNode, expanded map[string]bool, now time.Time) []sidebarNode {
	for _, w := range worktrees {
		out = append(out, w)
		if !expanded[w.Key()] {
			continue
		}
		recent, older := PartitionSessionsByRecency(w.Sessions)
		for _, s := range recent {
			out = append(out, sessionNode{Session: s, ParentPath: w.LocalPath})
		}
		if len(older) > 0 {
			bucket := olderSessionsNode{ParentPath: w.LocalPath, Hidden: older}
			out = append(out, bucket)
			if expanded[bucket.Key()] {
				for _, s := range older {
					out = append(out, sessionNode{Session: s, ParentPath: w.LocalPath})
				}
			}
		}
	}
	return out
}
