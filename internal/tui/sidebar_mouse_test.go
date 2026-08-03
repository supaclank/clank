package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
)

// sidebarWithRows builds a sidebar populated with N sessions across
// distinct worktrees so m.flat has at least the requested number of
// rows. Returns the sidebar and the row count actually produced.
func sidebarWithRows(t *testing.T, n int) (*SidebarModel, int) {
	t.Helper()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	sessions := make([]agent.SessionInfo, n)
	for i := 0; i < n; i++ {
		sessions[i] = agent.SessionInfo{
			ID:        string(rune('a'+i)) + "-id",
			UpdatedAt: now,
			GitRef:    agent.GitRef{LocalPath: "/r/" + string(rune('a'+i))},
		}
	}
	m := newTestSidebar(now, sessions...)
	if len(m.flat) < 2 {
		t.Fatalf("test setup: need at least 2 flat rows, got %d", len(m.flat))
	}
	return &m, len(m.flat)
}

func TestSidebarHandleWheel_DownMovesCursorByOne(t *testing.T) {
	t.Parallel()
	m, _ := sidebarWithRows(t, 5)
	m.cursor = 0

	if !m.HandleWheel(tea.MouseWheelDown) {
		t.Fatal("wheel-down at cursor=0 should move cursor")
	}
	if m.cursor != 1 {
		t.Errorf("after wheel-down: cursor=%d, want 1", m.cursor)
	}
}

func TestSidebarHandleWheel_UpMovesCursorByOne(t *testing.T) {
	t.Parallel()
	m, _ := sidebarWithRows(t, 5)
	m.cursor = 3

	if !m.HandleWheel(tea.MouseWheelUp) {
		t.Fatal("wheel-up at cursor=3 should move cursor")
	}
	if m.cursor != 2 {
		t.Errorf("after wheel-up: cursor=%d, want 2", m.cursor)
	}
}

func TestSidebarHandleWheel_ClampsAtTopNoWrap(t *testing.T) {
	t.Parallel()
	m, _ := sidebarWithRows(t, 5)
	m.cursor = 0

	moved := m.HandleWheel(tea.MouseWheelUp)
	if moved {
		t.Error("wheel-up at top should not move cursor")
	}
	if m.cursor != 0 {
		t.Errorf("after wheel-up at top: cursor=%d, want 0 (no wraparound)", m.cursor)
	}
}

func TestSidebarHandleWheel_ClampsAtBottomNoWrap(t *testing.T) {
	t.Parallel()
	m, rows := sidebarWithRows(t, 5)
	m.cursor = rows - 1

	moved := m.HandleWheel(tea.MouseWheelDown)
	if moved {
		t.Error("wheel-down at bottom should not move cursor")
	}
	if m.cursor != rows-1 {
		t.Errorf("after wheel-down at bottom: cursor=%d, want %d (no wraparound)", m.cursor, rows-1)
	}
}
