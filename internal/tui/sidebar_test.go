package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

// TestSidebar_SectionBreakpoints verifies the breakpoint list adapts to the
// number of entries:
//
//   - 0 entries: [0 (All), 1 (Import), 2 (Cloud), 3 (Settings)]
//   - 1 entry:   [0, 1, 2, 3, 4]
//   - 3 entries: [0, 1, 3, 4, 5, 6]
func TestSidebar_SectionBreakpoints(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		entries int
		want    []int
	}{
		{"no entries", 0, []int{0, 1, 2, 3}},
		{"one entry", 1, []int{0, 1, 2, 3, 4}},
		{"three entries", 3, []int{0, 1, 3, 4, 5, 6}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := SidebarModel{entries: makeEntries(tc.entries)}
			got := m.sectionBreakpoints()
			if !equalInts(got, tc.want) {
				t.Errorf("sectionBreakpoints(%d entries) = %v, want %v", tc.entries, got, tc.want)
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
		entries    int
		startCur   int
		key        tea.KeyPressMsg
		wantCursor int
	}{
		// 3 entries → breakpoints [0, 1, 3, 4, 5, 6]
		{"3e: shift+down from All -> first entry", 3, 0, shiftDownKey(), 1},
		{"3e: shift+down from first entry -> end of worktrees", 3, 1, shiftDownKey(), 3},
		{"3e: shift+down from middle -> end of worktrees", 3, 2, shiftDownKey(), 3},
		{"3e: shift+down from end of worktrees -> Import", 3, 3, shiftDownKey(), 4},
		{"3e: shift+down from Import -> Cloud", 3, 4, shiftDownKey(), 5},
		{"3e: shift+down from Cloud -> Settings", 3, 5, shiftDownKey(), 6},
		{"3e: shift+down from Settings clamps", 3, 6, shiftDownKey(), 6},
		{"3e: shift+up from Settings -> Cloud", 3, 6, shiftUpKey(), 5},
		{"3e: shift+up from Cloud -> Import", 3, 5, shiftUpKey(), 4},
		{"3e: shift+up from Import -> end of worktrees", 3, 4, shiftUpKey(), 3},
		{"3e: shift+up from end of worktrees -> first entry", 3, 3, shiftUpKey(), 1},
		{"3e: shift+up from middle -> first entry", 3, 2, shiftUpKey(), 1},
		{"3e: shift+up from first entry -> All", 3, 1, shiftUpKey(), 0},
		{"3e: shift+up from All clamps", 3, 0, shiftUpKey(), 0},

		// 0 entries → breakpoints [0, 1, 2, 3]
		{"0e: shift+down from All -> Import", 0, 0, shiftDownKey(), 1},
		{"0e: shift+down from Import -> Cloud", 0, 1, shiftDownKey(), 2},
		{"0e: shift+down from Cloud -> Settings", 0, 2, shiftDownKey(), 3},
		{"0e: shift+up from Settings -> Cloud", 0, 3, shiftUpKey(), 2},
		{"0e: shift+up from Cloud -> Import", 0, 2, shiftUpKey(), 1},
		{"0e: shift+up from Import -> All", 0, 1, shiftUpKey(), 0},

		// 1 entry → breakpoints [0, 1, 2, 3, 4]
		{"1e: shift+down from All -> entry", 1, 0, shiftDownKey(), 1},
		{"1e: shift+down from entry -> Import", 1, 1, shiftDownKey(), 2},
		{"1e: shift+down from Import -> Cloud", 1, 2, shiftDownKey(), 3},
		{"1e: shift+down from Cloud -> Settings", 1, 3, shiftDownKey(), 4},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := SidebarModel{
				entries: makeEntries(tc.entries),
				cursor:  tc.startCur,
				focused: true,
				height:  20,
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
		entries: makeEntries(20),
		cursor:  0,
		focused: true,
		height:  8, // listHeight clamps small but viewport stays smaller than list
	}
	m.handleKey(shiftDownKey()) // All → first entry (cursor=1)
	m.handleKey(shiftDownKey()) // first entry → last entry (cursor=20)
	if m.cursor != 20 {
		t.Fatalf("cursor: got %d, want 20", m.cursor)
	}
	if m.scroll == 0 {
		t.Errorf("expected scroll to advance from 0 after shift+down jump, got 0")
	}
}

// --- helpers ---

func upKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyUp}
}

func downKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyDown}
}

// TestSidebar_ArrowNavigationWraps verifies that pressing up at the top
// wraps to the bottom (Settings row) and pressing down at the bottom wraps
// to the top (All row).
func TestSidebar_ArrowNavigationWraps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		entries    int
		startCur   int
		key        tea.KeyPressMsg
		wantCursor int
	}{
		// 3 entries → maxIdx = 6 (Settings row)
		{"3e: up from All wraps to Settings", 3, 0, upKey(), 6},
		{"3e: down from Settings wraps to All", 3, 6, downKey(), 0},
		{"3e: up from middle moves up", 3, 2, upKey(), 1},
		{"3e: down from middle moves down", 3, 2, downKey(), 3},

		// 0 entries → maxIdx = 3
		{"0e: up from All wraps to Settings", 0, 0, upKey(), 3},
		{"0e: down from Settings wraps to All", 0, 3, downKey(), 0},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := SidebarModel{
				entries: makeEntries(tc.entries),
				cursor:  tc.startCur,
				focused: true,
				height:  20,
			}
			m.handleKey(tc.key)
			if m.cursor != tc.wantCursor {
				t.Errorf("cursor after key: got %d, want %d", m.cursor, tc.wantCursor)
			}
		})
	}
}

func makeEntries(n int) []worktreeEntry {
	out := make([]worktreeEntry, n)
	for i := 0; i < n; i++ {
		out[i] = worktreeEntry{
			LocalPath: "/repo/" + entryName(i),
			Label:     entryName(i),
		}
	}
	return out
}

func entryName(i int) string {
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

// TestSidebar_BranchWorktreeCreatedClearsStaleErr is a regression test:
// after a failed ResolveWorktree set m.err, a subsequent successful
// branchWorktreeCreatedMsg must clear it so the old error doesn't render
// while the new composing session opens.
func TestSidebar_BranchWorktreeCreatedClearsStaleErr(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		err:      errors.New("prior resolve failure"),
		creating: true,
	}

	cmd := m.Update(branchWorktreeCreatedMsg{worktreeDir: "/tmp/wt"})

	if m.err != nil {
		t.Errorf("expected m.err cleared on success, got %v", m.err)
	}
	if m.creating {
		t.Errorf("expected creating=false on success")
	}
	if cmd == nil {
		t.Fatalf("expected non-nil cmd emitting newWorktreeSessionRequestMsg")
	}
	got, ok := cmd().(newWorktreeSessionRequestMsg)
	if !ok {
		t.Fatalf("expected newWorktreeSessionRequestMsg, got %T", cmd())
	}
	if got.worktreeDir != "/tmp/wt" {
		t.Errorf("worktreeDir: got %q, want %q", got.worktreeDir, "/tmp/wt")
	}
}

// TestSidebar_SetSessionsClampsScroll is a regression test: when the
// session list shrinks below m.scroll, renderWorktreeEntries would slice
// m.entries[m.scroll:end] with m.scroll > end and panic. SetSessions
// must clamp m.scroll alongside m.cursor.
func TestSidebar_SetSessionsClampsScroll(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tenSessions := make([]agent.SessionInfo, 10)
	for i := range tenSessions {
		tenSessions[i] = agent.SessionInfo{
			GitRef:    agent.GitRef{LocalPath: "/repo/" + entryName(i)},
			UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}
	}

	m := SidebarModel{height: 20, focused: true}
	m.SetSessions(tenSessions)
	if len(m.entries) != 10 {
		t.Fatalf("entries: got %d, want 10", len(m.entries))
	}
	m.scroll = 5

	twoSessions := []agent.SessionInfo{
		{GitRef: agent.GitRef{LocalPath: "/repo/a"}, UpdatedAt: now},
		{GitRef: agent.GitRef{LocalPath: "/repo/b"}, UpdatedAt: now},
	}
	m.SetSessions(twoSessions)

	if len(m.entries) != 2 {
		t.Fatalf("entries after shrink: got %d, want 2", len(m.entries))
	}
	if m.scroll > len(m.entries)-1 {
		t.Errorf("scroll not clamped: got %d, want <= %d", m.scroll, len(m.entries)-1)
	}

	// View() must not panic with the now-shrunken state.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("View panicked after shrink: %v", r)
		}
	}()
	_ = m.View()
}

// TestSidebar_SetSessionsClampsScrollToZeroOnEmpty covers the empty case.
func TestSidebar_SetSessionsClampsScrollToZeroOnEmpty(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		entries: makeEntries(5),
		scroll:  3,
	}
	m.SetSessions(nil)
	if m.scroll != 0 {
		t.Errorf("scroll: got %d, want 0", m.scroll)
	}
}

// TestSidebar_CreateOnLastVisibleEntry_KeepsCursorAndInputVisible is a
// regression for a bug where pressing 'n' on the last visible entry of a
// scrollable sidebar made the cursor's row and the input row both vanish:
// entering creating mode shrinks entryViewportH but ensureVisible was not
// called, so the cursor entry fell outside entries[scroll:scroll+vh].
func TestSidebar_CreateOnLastVisibleEntry_KeepsCursorAndInputVisible(t *testing.T) {
	t.Parallel()

	// height=18 → listHeight=14, vh=6 normally, vh=5 when creating on an
	// entry. With 20 entries the list is scrollable.
	m := NewSidebarModel(nil, "local", agent.GitRef{LocalPath: "/tmp"}, "/tmp")
	m.entries = makeEntries(20)
	m.focused = true
	m.width = 30
	m.height = 18
	m.cursor = 6 // last visible entry (entries[5], label "bf") at scroll=0

	if vh := m.entryViewportH(); vh != 6 {
		t.Fatalf("precondition: vh=%d, want 6", vh)
	}

	m.handleKey(tea.KeyPressMsg{Text: "n", Code: 'n'})

	if !m.creating {
		t.Fatal("expected creating=true after 'n'")
	}

	out := m.View()
	wantLabel := entryName(5) // "bf"
	if !strings.Contains(out, wantLabel) {
		t.Errorf("cursor entry %q missing from view after 'n':\n%s", wantLabel, out)
	}
	// The new-branch input shows a placeholder ending in "ranch-name"
	// (the leading "b" gets wrapped in cursor-highlight ANSI codes).
	if !strings.Contains(out, "ranch-name") {
		t.Errorf("input row missing from view after 'n':\n%s", out)
	}
}

// TestSidebar_CreateMovesSelectionMarkerToInputRow verifies the "> " marker
// follows where the user is typing: while creating, the parent entry no
// longer carries the marker and the input row carries it instead.
func TestSidebar_CreateMovesSelectionMarkerToInputRow(t *testing.T) {
	t.Parallel()

	m := NewSidebarModel(nil, "local", agent.GitRef{LocalPath: "/tmp"}, "/tmp")
	m.entries = makeEntries(3)
	m.focused = true
	m.width = 30
	m.height = 24
	m.cursor = 1 // first entry "bb"

	beforeLines := splitVisibleLines(m.View())
	parentBefore := lineContaining(beforeLines, entryName(0))
	if !strings.Contains(parentBefore, "> ") {
		t.Fatalf("precondition: parent row should carry '> ' before 'n':\n%s", parentBefore)
	}

	m.handleKey(tea.KeyPressMsg{Text: "n", Code: 'n'})

	afterLines := splitVisibleLines(m.View())
	parentAfter := lineContaining(afterLines, entryName(0))
	if strings.Contains(parentAfter, "> ") {
		t.Errorf("parent row should not carry '> ' while creating:\n%s", parentAfter)
	}
	inputRow := lineContaining(afterLines, "ranch-name")
	if !strings.Contains(inputRow, "> ") {
		t.Errorf("input row should carry '> ' while creating:\n%s", inputRow)
	}
}

// splitVisibleLines splits a rendered view into raw lines (ANSI preserved).
func splitVisibleLines(out string) []string {
	return strings.Split(out, "\n")
}

// lineContaining returns the first line containing substr, or "" if none.
func lineContaining(lines []string, substr string) string {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return l
		}
	}
	return ""
}
