package tui

import (
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

func TestBuildSidebarTree_SortsByLatestActivity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	sessions := []agent.SessionInfo{
		{ID: "a-old", UpdatedAt: now.Add(-3 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/a"}},
		{ID: "b-new", UpdatedAt: now.Add(-30 * time.Minute), GitRef: agent.GitRef{LocalPath: "/r/b"}},
		{ID: "c-mid", UpdatedAt: now.Add(-2 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/c"}},
	}
	tree := buildSidebarTree(sessions, "", now)

	if got := pathsOf(tree.RecentWorktrees); !equalStrings(got, []string{"/r/b", "/r/c", "/r/a"}) {
		t.Errorf("RecentWorktrees order: got %v, want [/r/b /r/c /r/a]", got)
	}
	if got := len(tree.OlderWorktrees.Hidden); got != 0 {
		t.Errorf("OlderWorktrees: expected empty, got %d", got)
	}
}

func TestBuildSidebarTree_PartitionsOlderBucket(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 5, 21, 14, 0, 0, 0, time.Local)
	// Mix of in-window and out-of-window worktrees; the out-of-window
	// ones fall into the overflow bucket. Window = RecentWindowDays
	// back from the most-recent worktree.
	sessions := []agent.SessionInfo{
		{ID: "a", UpdatedAt: today, GitRef: agent.GitRef{LocalPath: "/r/0"}},
		{ID: "b", UpdatedAt: today.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/1"}},
		{ID: "c", UpdatedAt: today.AddDate(0, 0, -3), GitRef: agent.GitRef{LocalPath: "/r/2"}},
		{ID: "d", UpdatedAt: today.AddDate(0, 0, -7), GitRef: agent.GitRef{LocalPath: "/r/3"}},
		{ID: "e", UpdatedAt: today.AddDate(0, 0, -8), GitRef: agent.GitRef{LocalPath: "/r/4"}},
		{ID: "f", UpdatedAt: today.AddDate(0, 0, -30), GitRef: agent.GitRef{LocalPath: "/r/5"}},
	}
	tree := buildSidebarTree(sessions, "", today)

	if got := pathsOf(tree.RecentWorktrees); !equalStrings(got, []string{"/r/0", "/r/1", "/r/2", "/r/3"}) {
		t.Errorf("RecentWorktrees: got %v", got)
	}
	if got := pathsOf(tree.OlderWorktrees.Hidden); !equalStrings(got, []string{"/r/4", "/r/5"}) {
		t.Errorf("OlderWorktrees.Hidden: got %v", got)
	}
}

func TestBuildSidebarTree_AggregatesCounts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	path := "/r/x"
	sessions := []agent.SessionInfo{
		{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: path}},
		{ID: "b", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: path}, Visibility: agent.VisibilityDone},
		{ID: "c", UpdatedAt: now.Add(-2 * time.Hour), GitRef: agent.GitRef{LocalPath: path}, Visibility: agent.VisibilityArchived},
		{ID: "d", UpdatedAt: now.Add(-3 * time.Hour), GitRef: agent.GitRef{LocalPath: path}},
	}
	tree := buildSidebarTree(sessions, "", now)
	if len(tree.RecentWorktrees) != 1 {
		t.Fatalf("expected 1 worktree, got %d", len(tree.RecentWorktrees))
	}
	w := tree.RecentWorktrees[0]
	if w.Total != 4 || w.Active != 2 || w.Done != 1 || w.Archived != 1 {
		t.Errorf("counts: total=%d active=%d done=%d archived=%d (want 4/2/1/1)", w.Total, w.Active, w.Done, w.Archived)
	}
	if !w.LatestUpdatedAt.Equal(now) {
		t.Errorf("LatestUpdatedAt: got %v, want %v", w.LatestUpdatedAt, now)
	}
}

func TestBuildSidebarTree_SortsSessionsWithinWorktreeNewestFirst(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	path := "/r/x"
	sessions := []agent.SessionInfo{
		{ID: "old", UpdatedAt: now.Add(-3 * time.Hour), GitRef: agent.GitRef{LocalPath: path}},
		{ID: "new", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: path}},
		{ID: "mid", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: path}},
	}
	tree := buildSidebarTree(sessions, "", now)
	if got := idsOf(tree.RecentWorktrees[0].Sessions); !equalStrings(got, []string{"new", "mid", "old"}) {
		t.Errorf("sessions order: got %v, want [new mid old]", got)
	}
}

func TestBuildSidebarTree_DropsSessionsWithoutLocalPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	sessions := []agent.SessionInfo{
		{ID: "stray", UpdatedAt: now},
		{ID: "kept", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	}
	tree := buildSidebarTree(sessions, "", now)
	if len(tree.RecentWorktrees) != 1 || tree.RecentWorktrees[0].LocalPath != "/r/x" {
		t.Errorf("expected only the LocalPath-bearing session to produce a worktree, got %+v", tree.RecentWorktrees)
	}
}
