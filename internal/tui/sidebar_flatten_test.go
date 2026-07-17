package tui

import (
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

func TestFlattenSidebar_CollapsedWorktreesYieldNoSessionRows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	tree := buildSidebarTree(twoWorktreesFixture(now), "", now)

	got := flattenSidebar(tree, map[string]bool{}, now)
	wantKeys := []string{
		"wt:/r/alpha",
		"wt:/r/beta",
		"footer:import", "footer:cloud", "footer:settings",
	}
	if keys := keysOf(got); !equalStrings(keys, wantKeys) {
		t.Errorf("flatten: got %v, want %v", keys, wantKeys)
	}
}

func TestFlattenSidebar_ExpandedWorktreeShowsSessions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	tree := buildSidebarTree(twoWorktreesFixture(now), "", now)

	expanded := map[string]bool{"wt:/r/alpha": true}
	got := flattenSidebar(tree, expanded, now)
	wantKeys := []string{
		"wt:/r/alpha",
		"s:a-new", "s:a-old",
		"wt:/r/beta",
		"footer:import", "footer:cloud", "footer:settings",
	}
	if keys := keysOf(got); !equalStrings(keys, wantKeys) {
		t.Errorf("flatten: got %v, want %v", keys, wantKeys)
	}
}

func TestFlattenSidebar_OlderWorktreeBucketHidesUntilExpanded(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 5, 21, 14, 0, 0, 0, time.Local)
	// Two worktrees within the recent window stay visible; the
	// out-of-window one collapses into the older bucket.
	sessions := []agent.SessionInfo{
		{ID: "s0", UpdatedAt: today, GitRef: agent.GitRef{LocalPath: "/r/recent1"}},
		{ID: "s1", UpdatedAt: today.AddDate(0, 0, -3), GitRef: agent.GitRef{LocalPath: "/r/recent2"}},
		{ID: "s2", UpdatedAt: today.AddDate(0, 0, -30), GitRef: agent.GitRef{LocalPath: "/r/older"}},
	}
	tree := buildSidebarTree(sessions, "", today)

	collapsed := flattenSidebar(tree, map[string]bool{}, today)
	if got := keysOf(collapsed); !equalStrings(got, []string{
		"wt:/r/recent1", "wt:/r/recent2",
		"older:wt",
		"footer:import", "footer:cloud", "footer:settings",
	}) {
		t.Errorf("collapsed older bucket: got %v", got)
	}

	expanded := flattenSidebar(tree, map[string]bool{"older:wt": true}, today)
	if got := keysOf(expanded); !equalStrings(got, []string{
		"wt:/r/recent1", "wt:/r/recent2",
		"older:wt",
		"wt:/r/older",
		"footer:import", "footer:cloud", "footer:settings",
	}) {
		t.Errorf("expanded older bucket: got %v", got)
	}
}

func TestFlattenSidebar_CwdWorktreeAlwaysVisible(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 5, 21, 14, 0, 0, 0, time.Local)
	// One worktree active today, plus the cwd worktree at 60 days
	// old. Without cwd pinning the cwd would be in "+ show more"; with
	// pinning it appears in the recent list.
	sessions := []agent.SessionInfo{
		{ID: "s0", UpdatedAt: today, GitRef: agent.GitRef{LocalPath: "/r/active"}},
		{ID: "s1", UpdatedAt: today.AddDate(0, 0, -60), GitRef: agent.GitRef{LocalPath: "/r/cwd"}},
	}
	tree := buildSidebarTree(sessions, "/r/cwd", today)
	rows := flattenSidebar(tree, map[string]bool{}, today)
	if got := keysOf(rows); !equalStrings(got, []string{
		"wt:/r/active", "wt:/r/cwd",
		"footer:import", "footer:cloud", "footer:settings",
	}) {
		t.Errorf("cwd worktree should be visible regardless of age, got %v", got)
	}
}

func TestFlattenSidebar_PerWorktreeOlderSessionsBucket(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	path := "/r/x"
	// Six sessions: top MaxRecentBeforeBucket are visible, the tail
	// folds into the per-worktree olderSessionsNode.
	sessions := []agent.SessionInfo{
		{ID: "s0", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: path}},
		{ID: "s1", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: path}},
		{ID: "s2", UpdatedAt: now.Add(-2 * time.Hour), GitRef: agent.GitRef{LocalPath: path}},
		{ID: "s3", UpdatedAt: now.Add(-3 * time.Hour), GitRef: agent.GitRef{LocalPath: path}},
		{ID: "s4", UpdatedAt: now.Add(-4 * time.Hour), GitRef: agent.GitRef{LocalPath: path}},
		{ID: "wayback", UpdatedAt: now.Add(-5 * time.Hour), GitRef: agent.GitRef{LocalPath: path}},
	}
	tree := buildSidebarTree(sessions, "", now)

	// Worktree expanded, per-worktree older bucket collapsed.
	rows := flattenSidebar(tree, map[string]bool{"wt:" + path: true}, now)
	want := []string{
		"wt:" + path,
		"s:s0", "s:s1", "s:s2", "s:s3", "s:s4",
		"older:s:" + path,
		"footer:import", "footer:cloud", "footer:settings",
	}
	if got := keysOf(rows); !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Now expand the older bucket too.
	rows = flattenSidebar(tree, map[string]bool{
		"wt:" + path:      true,
		"older:s:" + path: true,
	}, now)
	want = []string{
		"wt:" + path,
		"s:s0", "s:s1", "s:s2", "s:s3", "s:s4",
		"older:s:" + path,
		"s:wayback",
		"footer:import", "footer:cloud", "footer:settings",
	}
	if got := keysOf(rows); !equalStrings(got, want) {
		t.Errorf("expanded session-older: got %v, want %v", got, want)
	}
}

func TestFlattenSidebar_EmptyOlderBucketsNotEmitted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	path := "/r/onlyfresh"
	sessions := []agent.SessionInfo{
		{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: path}},
		{ID: "b", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: path}},
	}
	tree := buildSidebarTree(sessions, "", now)

	rows := flattenSidebar(tree, map[string]bool{"wt:" + path: true}, now)
	for _, n := range rows {
		if n.Kind() == nodeOlderSessions {
			t.Fatalf("did not expect olderSessionsNode when no sessions are old, got %v", keysOf(rows))
		}
		if n.Kind() == nodeOlderWorktrees {
			t.Fatalf("did not expect olderWorktreesNode when no worktrees are old, got %v", keysOf(rows))
		}
	}
}

func twoWorktreesFixture(now time.Time) []agent.SessionInfo {
	return []agent.SessionInfo{
		{ID: "a-new", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/alpha"}},
		{ID: "a-old", UpdatedAt: now.Add(-2 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/alpha"}},
		{ID: "b-only", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/beta"}},
	}
}

func keysOf(nodes []sidebarNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Key()
	}
	return out
}
