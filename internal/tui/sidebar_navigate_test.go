package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

// fixedNow lets tests pin the sidebar's clock so age-based bucketing
// is deterministic regardless of when the test runs.
func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func newTestSidebar(now time.Time, sessions ...agent.SessionInfo) SidebarModel {
	return newTestSidebarCwd(now, "", sessions...)
}

// newTestSidebarCwd is the cwd-aware variant: cwdLocalPath becomes the
// "current worktree" the sidebar pins (always visible + auto-expanded).
func newTestSidebarCwd(now time.Time, cwdLocalPath string, sessions ...agent.SessionInfo) SidebarModel {
	m := NewSidebarModel(nil, "", agent.GitRef{LocalPath: cwdLocalPath}, "")
	m.nowFn = fixedNow(now)
	m.focused = true
	m.SetSessions(sessions)
	return m
}

func TestSidebar_DefaultCursorIsAllSessions(t *testing.T) {
	t.Parallel()
	m := newTestSidebar(time.Now())
	if k := m.cursorNodeKind(); k != nodeAllSessions {
		t.Errorf("expected default cursor on AllSessions, got kind %d", k)
	}
	if dir := m.SelectedWorktreeDir(); dir != "" {
		t.Errorf("SelectedWorktreeDir on AllSessions should be empty, got %q", dir)
	}
}

func TestSidebar_EnterOnWorktreeTogglesExpand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	// Default: non-cwd worktrees start collapsed. Enter expands.
	m.cursor = 1
	if m.expanded["wt:/r/x"] {
		t.Fatalf("precondition: non-cwd worktree should start collapsed")
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.expanded["wt:/r/x"] {
		t.Errorf("expected worktree to expand after Enter")
	}
	if got := keysOf(m.flat); !containsKey(got, "s:a") {
		t.Errorf("expected session row to appear after expand, got %v", got)
	}
}

func TestSidebar_EnterOnSessionEmitsSelection(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	// Cwd matches the worktree, so it auto-expands and the session
	// row is visible in the flat list without manual expansion.
	m := newTestSidebarCwd(now, "/r/x",
		agent.SessionInfo{ID: "abc123", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	m.cursor = 2 // session row

	cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("expected a cmd from Enter on session, got nil")
	}
	got, ok := cmd().(sessionSelectedFromSidebarMsg)
	if !ok {
		t.Fatalf("expected sessionSelectedFromSidebarMsg, got %T", cmd())
	}
	if got.sessionID != "abc123" {
		t.Errorf("expected sessionID=abc123, got %q", got.sessionID)
	}
}

func TestSidebar_ShiftEnter_StartsAtFirstUnread(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	// Three sessions, the middle one is unread.
	read := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "s0", UpdatedAt: now, LastReadAt: read, GitRef: agent.GitRef{LocalPath: "/r/x"}},
		{ID: "s1", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/x"}}, // unread
		{ID: "s2", UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: read, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	}
	m := newTestSidebar(now, sessions...)
	m.cursor = 1 // worktree row

	cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	if cmd == nil {
		t.Fatalf("expected a cmd from Shift+Enter")
	}
	msg := cmd().(sessionSelectedFromSidebarMsg)
	if msg.sessionID != "s1" {
		t.Errorf("first Shift+Enter should land on the first unread (s1), got %q", msg.sessionID)
	}
}

func TestSidebar_ShiftEnter_AdvancesAndWraps(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	sessions := []agent.SessionInfo{
		{ID: "s0", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
		{ID: "s1", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/x"}},
		{ID: "s2", UpdatedAt: now.Add(-2 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/x"}},
	}
	m := newTestSidebar(now, sessions...)
	m.cursor = 1

	gotIDs := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
		gotIDs = append(gotIDs, cmd().(sessionSelectedFromSidebarMsg).sessionID)
	}
	// All sessions unread → firstUnreadIndex returns 0. So sequence is
	// s0, s1, s2, s0, s1.
	want := []string{"s0", "s1", "s2", "s0", "s1"}
	for i, id := range gotIDs {
		if id != want[i] {
			t.Errorf("press %d: got %q, want %q", i, id, want[i])
		}
	}
}

func TestSidebar_ShiftEnter_OnNonWorktreeIsNoOp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	m.cursor = 0 // AllSessions row
	cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	if cmd != nil {
		t.Errorf("expected no cmd from Shift+Enter on AllSessions, got %v", cmd())
	}
}

func TestSidebar_TabTogglesExpand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	// Non-cwd worktrees default collapsed. Tab expands; Tab again
	// collapses.
	m.cursor = 1
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if !m.expanded["wt:/r/x"] {
		t.Errorf("expected Tab to expand the worktree")
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.expanded["wt:/r/x"] {
		t.Errorf("expected Tab again to collapse the worktree")
	}
}

func TestSidebar_PersistedExpandedSeedsState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := NewSidebarModel(nil, "", agent.GitRef{}, "")
	m.nowFn = fixedNow(now)
	m.SetExpanded(map[string]bool{
		"wt:/r/x":   true,
		"older:wt":  true, // should be stripped (always collapses on load)
		"older:s:/": true, // ditto
	})
	m.SetSessions([]agent.SessionInfo{
		{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	})
	if !m.expanded["wt:/r/x"] {
		t.Errorf("expected wt:/r/x to be expanded from seed")
	}
	if m.expanded["older:wt"] {
		t.Errorf("expected older:wt to be stripped from persisted seed")
	}
	if m.expanded["older:s:/"] {
		t.Errorf("expected older:s:/ to be stripped from persisted seed")
	}
}

func TestSidebar_SnapshotExpandedDropsOlderBuckets(t *testing.T) {
	t.Parallel()
	m := NewSidebarModel(nil, "", agent.GitRef{}, "")
	m.userToggles = map[string]bool{
		"wt:/r/x":      true,
		"older:wt":     true,
		"older:s:/r/x": true,
	}
	snap := m.SnapshotExpanded()
	if !snap["wt:/r/x"] {
		t.Errorf("expected wt:/r/x to persist")
	}
	if _, ok := snap["older:wt"]; ok {
		t.Errorf("expected older:wt to be dropped from snapshot")
	}
	if _, ok := snap["older:s:/r/x"]; ok {
		t.Errorf("expected older:s:/r/x to be dropped from snapshot")
	}
}

func TestSidebar_StaleExpandedKeysArePruned(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	m.userToggles["wt:/r/gone"] = true
	m.rebuildTree()
	if _, ok := m.userToggles["wt:/r/gone"]; ok {
		t.Errorf("expected stale wt:/r/gone key to be pruned after rebuildTree")
	}
}

func TestSidebar_CwdWorktreeAutoExpands(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebarCwd(now, "/r/x",
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	if !m.expanded["wt:/r/x"] {
		t.Errorf("expected cwd worktree to auto-expand")
	}
	// Auto-expand should NOT be persisted (the user never toggled).
	if _, ok := m.userToggles["wt:/r/x"]; ok {
		t.Errorf("auto-expand should not populate userToggles, got %v", m.userToggles)
	}
	if snap := m.SnapshotExpanded(); len(snap) != 0 {
		t.Errorf("snapshot should be empty when only auto-defaults are in effect, got %v", snap)
	}
}

func TestSidebar_NonCwdWorktreesStayCollapsed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	// Cwd is /r/x; /r/y is just another visible worktree. Only /r/x
	// should auto-expand.
	m := newTestSidebarCwd(now, "/r/x",
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
		agent.SessionInfo{ID: "b", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/y"}},
	)
	if !m.expanded["wt:/r/x"] {
		t.Errorf("expected cwd worktree to auto-expand")
	}
	if m.expanded["wt:/r/y"] {
		t.Errorf("non-cwd worktree should stay collapsed by default")
	}
}

func TestSidebar_UserCollapseBeatsAutoExpand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebarCwd(now, "/r/x",
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	m.cursor = 1
	// Cwd auto-expanded → Enter collapses → user toggle = false.
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.expanded["wt:/r/x"] {
		t.Fatalf("precondition: expected collapsed after first Enter")
	}
	// Snapshot persists the explicit collapse so future launches keep it.
	snap := m.SnapshotExpanded()
	if v, ok := snap["wt:/r/x"]; !ok || v {
		t.Errorf("snapshot should persist explicit collapse, got %v", snap)
	}
}

func TestSidebar_CursorOnFooterRows(t *testing.T) {
	t.Parallel()
	m := newTestSidebar(time.Now())
	// flat is: AllSessions, Import, Cloud, Settings (no worktrees in fixture).
	if len(m.flat) != 4 {
		t.Fatalf("expected 4 nodes, got %d (%v)", len(m.flat), keysOf(m.flat))
	}
	m.cursor = 1
	if !m.CursorOnImport() {
		t.Errorf("expected CursorOnImport at idx 1")
	}
	m.cursor = 2
	if !m.CursorOnCloud() {
		t.Errorf("expected CursorOnCloud at idx 2")
	}
	m.cursor = 3
	if !m.CursorOnSettings() {
		t.Errorf("expected CursorOnSettings at idx 3")
	}
}

func containsKey(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
