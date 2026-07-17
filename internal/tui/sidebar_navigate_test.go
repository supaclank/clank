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

func TestSidebar_DefaultCursorIsFirstRow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	// With a worktree present, the default cursor (index 0) lands on the
	// first worktree row now that the "All sessions" row is gone.
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	if m.cursor != 0 {
		t.Errorf("expected default cursor at index 0, got %d", m.cursor)
	}
	if k := m.cursorNodeKind(); k != nodeWorktree {
		t.Errorf("expected default cursor on first worktree, got kind %d", k)
	}
}

func TestSidebar_EnterOnWorktreeTogglesExpand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	// Default: non-cwd worktrees start collapsed. Enter expands.
	m.cursor = 0 // worktree row (first row now that "All sessions" is gone)
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
	m.cursor = 1 // session row (worktree at 0, its session at 1)

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
	// Three sessions, the middle one is unread. read is anchored to
	// `now` (not the wall clock) so the test stays deterministic.
	read := now.Add(time.Minute)
	sessions := []agent.SessionInfo{
		{ID: "s0", UpdatedAt: now, LastReadAt: read, GitRef: agent.GitRef{LocalPath: "/r/x"}},
		{ID: "s1", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/x"}}, // unread
		{ID: "s2", UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: read, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	}
	m := newTestSidebar(now, sessions...)
	m.cursor = 0 // worktree row

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
	m.cursor = 0 // worktree row

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
	// flat is: [worktree /r/x, Import, Cloud, Settings]. Park on Import
	// (a non-worktree footer row) — Shift+Enter there must be a no-op.
	m.cursor = 1
	if !m.CursorOnImport() {
		t.Fatalf("setup: expected cursor on Import at idx 1, got kind %d", m.cursorNodeKind())
	}
	cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	if cmd != nil {
		t.Errorf("expected no cmd from Shift+Enter on a non-worktree row, got %v", cmd())
	}
}

func TestSidebar_SpaceTogglesExpand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	// Non-cwd worktrees default collapsed. Space expands without
	// moving the cursor; Space again collapses. (Tab is reserved for
	// pane switching at the inbox level and intentionally does NOT
	// reach sidebar.handleKey.)
	m.cursor = 0 // worktree row
	m.handleKey(tea.KeyPressMsg{Code: tea.KeySpace})
	if !m.expanded["wt:/r/x"] {
		t.Errorf("expected Space to expand the worktree")
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeySpace})
	if m.expanded["wt:/r/x"] {
		t.Errorf("expected Space again to collapse the worktree")
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
	m.cursor = 0 // cwd worktree row
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

func TestSidebar_ShiftJump_WalksWorktreesRegardlessOfExpandState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/wt1"}},
		agent.SessionInfo{ID: "b", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/wt2"}},
		agent.SessionInfo{ID: "c", UpdatedAt: now.Add(-2 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/wt3"}},
	)
	// Open the middle worktree so its session appears between wt1 and
	// wt3 in the flat list. shift+down from wt1 should skip over the
	// session and land on wt2; another shift+down should land on wt3.
	m.userToggles["wt:/r/wt2"] = true
	m.rebuildFlat()

	for i, n := range m.flat {
		if n.Key() == "wt:/r/wt1" {
			m.cursor = i
			break
		}
	}
	m.shiftJump(true)
	if got := m.flat[m.cursor].Key(); got != "wt:/r/wt2" {
		t.Fatalf("shift+down from wt1 should land on wt2, got %q", got)
	}
	m.shiftJump(true)
	if got := m.flat[m.cursor].Key(); got != "wt:/r/wt3" {
		t.Errorf("shift+down from open wt2 should skip its session and land on wt3, got %q", got)
	}
}

func TestSidebar_ShiftJump_SymmetricAndCommutative(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/wt1"}},
		agent.SessionInfo{ID: "b", UpdatedAt: now.Add(-1 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/wt2"}},
		agent.SessionInfo{ID: "c", UpdatedAt: now.Add(-2 * time.Hour), GitRef: agent.GitRef{LocalPath: "/r/wt3"}},
	)
	m.userToggles["wt:/r/wt2"] = true
	m.rebuildFlat()

	// Park on wt2. shift+down → wt3, shift+up should return to wt2.
	for i, n := range m.flat {
		if n.Key() == "wt:/r/wt2" {
			m.cursor = i
			break
		}
	}
	start := m.cursor
	m.shiftJump(true)
	m.shiftJump(false)
	if m.cursor != start {
		t.Errorf("shift+down then shift+up should be a no-op, started at %d ended at %d (key %q)",
			start, m.cursor, m.flat[m.cursor].Key())
	}
}

func TestSidebar_ShiftJump_FallsBackToSectionAnchorsAtEdges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebar(now,
		agent.SessionInfo{ID: "a", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/only"}},
	)
	// flat is: [worktree /r/only, Import, Cloud, Settings]. Park on the
	// only worktree (index 0). There's no row above it (the "All sessions"
	// anchor is gone), so shift+up stays put.
	m.cursor = 0
	m.shiftJump(false)
	if k := m.flat[m.cursor].Kind(); k != nodeWorktree {
		t.Errorf("shift+up from the first worktree should stay on it, got kind %d", k)
	}
	// Reset cursor on the worktree and try shift+down: no more
	// worktrees below → falls through to the first footer row.
	m.cursor = 0
	m.shiftJump(true)
	if k := m.flat[m.cursor].Kind(); k != nodeImport {
		t.Errorf("shift+down past last worktree should land on Import (first footer), got kind %d", k)
	}
}

func TestSidebar_CursorOnFooterRows(t *testing.T) {
	t.Parallel()
	m := newTestSidebar(time.Now())
	// flat is: Import, Cloud, Settings (no worktrees in fixture).
	if len(m.flat) != 3 {
		t.Fatalf("expected 3 nodes, got %d (%v)", len(m.flat), keysOf(m.flat))
	}
	m.cursor = 0
	if !m.CursorOnImport() {
		t.Errorf("expected CursorOnImport at idx 0")
	}
	m.cursor = 1
	if !m.CursorOnCloud() {
		t.Errorf("expected CursorOnCloud at idx 1")
	}
	m.cursor = 2
	if !m.CursorOnSettings() {
		t.Errorf("expected CursorOnSettings at idx 2")
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
