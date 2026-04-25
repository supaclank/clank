package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/host"
)

// TestSidebar_SectionBreakpoints verifies the breakpoint list adapts to the
// number of branches:
//
//   - 0 branches: [0 (All), 1 (Settings)]   — no separate "end of worktrees"
//   - 1 branch:   [0, 1, 2]
//   - 3 branches: [0, 3, 4]
func TestSidebar_SectionBreakpoints(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		branches int
		want     []int
	}{
		{"no branches", 0, []int{0, 1}},
		{"one branch", 1, []int{0, 1, 2}},
		{"three branches", 3, []int{0, 3, 4}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := SidebarModel{branches: makeBranches(tc.branches)}
			got := m.sectionBreakpoints()
			if !equalInts(got, tc.want) {
				t.Errorf("sectionBreakpoints(%d branches) = %v, want %v", tc.branches, got, tc.want)
			}
		})
	}
}

// TestSidebar_ShiftArrowNavigation exercises shift+up / shift+down via
// handleKey so we cover binding wiring as well as the breakpoint math.
func TestSidebar_ShiftArrowNavigation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		branches   int
		startCur   int
		key        tea.KeyPressMsg
		wantCursor int
	}{
		// 3 branches → breakpoints [0, 3, 4]
		{"3b: shift+down from All -> end of worktrees", 3, 0, shiftDownKey(), 3},
		{"3b: shift+down from middle -> end of worktrees", 3, 2, shiftDownKey(), 3},
		{"3b: shift+down from end of worktrees -> Settings", 3, 3, shiftDownKey(), 4},
		{"3b: shift+down from Settings clamps", 3, 4, shiftDownKey(), 4},
		{"3b: shift+up from Settings -> end of worktrees", 3, 4, shiftUpKey(), 3},
		{"3b: shift+up from end of worktrees -> All", 3, 3, shiftUpKey(), 0},
		{"3b: shift+up from middle -> All", 3, 1, shiftUpKey(), 0},
		{"3b: shift+up from All clamps", 3, 0, shiftUpKey(), 0},

		// 0 branches → breakpoints [0, 1]
		{"0b: shift+down from All -> Settings", 0, 0, shiftDownKey(), 1},
		{"0b: shift+up from Settings -> All", 0, 1, shiftUpKey(), 0},

		// 1 branch → breakpoints [0, 1, 2]
		{"1b: shift+down from All -> branch", 1, 0, shiftDownKey(), 1},
		{"1b: shift+down from branch -> Settings", 1, 1, shiftDownKey(), 2},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := SidebarModel{
				branches: makeBranches(tc.branches),
				cursor:   tc.startCur,
				focused:  true,
				height:   20,
			}
			m.handleKey(tc.key)
			if m.cursor != tc.wantCursor {
				t.Errorf("cursor after key: got %d, want %d", m.cursor, tc.wantCursor)
			}
		})
	}
}

// TestSidebar_ShiftDown_EnsuresVisible checks ensureVisible runs after a
// shift+down jump, by giving the sidebar a tiny height so Settings is
// off-screen from cursor=0 and verifying scroll moves.
func TestSidebar_ShiftDown_EnsuresVisible(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		branches: makeBranches(20),
		cursor:   0,
		focused:  true,
		height:   8, // listHeight clamps small but viewport stays smaller than list
	}
	m.handleKey(shiftDownKey())
	if m.cursor != 20 { // end of worktrees
		t.Fatalf("cursor: got %d, want 20", m.cursor)
	}
	if m.scroll == 0 {
		t.Errorf("expected scroll to advance from 0 after shift+down jump, got 0")
	}
}

// --- helpers ---

func makeBranches(n int) []host.BranchInfo {
	out := make([]host.BranchInfo, n)
	for i := 0; i < n; i++ {
		out[i] = host.BranchInfo{Name: branchName(i)}
	}
	return out
}

func branchName(i int) string {
	return "b" + string(rune('a'+i))
}

func shiftUpKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}
}

func shiftDownKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
