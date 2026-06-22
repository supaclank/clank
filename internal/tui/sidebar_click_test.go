package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/acksell/clank/internal/agent"
)

// --- Hit-map: a rendered screen row resolves to the node drawn there ---

func TestSidebarNodeAtRow_MapsRenderedRowsToNodes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m := newTestSidebarCwd(now, "/r/x",
		agent.SessionInfo{ID: "s1", Title: "AlphaSession", GitRef: agent.GitRef{LocalPath: "/r/x"}, UpdatedAt: now},
	)
	m.SetSize(34, 40)
	lines := strings.Split(m.View(), "\n")

	rowOf := func(sub string) int {
		t.Helper()
		for i, l := range lines {
			if strings.Contains(ansi.Strip(l), sub) {
				return i
			}
		}
		t.Fatalf("row containing %q not found in sidebar render", sub)
		return -1
	}
	kindAt := func(sub string) sidebarNodeKind {
		t.Helper()
		idx := m.NodeAtRow(rowOf(sub))
		if idx < 0 || idx >= len(m.flat) {
			t.Fatalf("NodeAtRow for %q returned out-of-range %d", sub, idx)
		}
		return m.flat[idx].Kind()
	}

	if k := kindAt("All sessions"); k != nodeAllSessions {
		t.Errorf("All sessions row → kind %d, want nodeAllSessions", k)
	}
	if k := kindAt("AlphaSession"); k != nodeSession {
		t.Errorf("session row → kind %d, want nodeSession", k)
	}
	if k := kindAt("Cloud"); k != nodeCloud {
		t.Errorf("cloud footer row → kind %d, want nodeCloud", k)
	}
	if k := kindAt("Settings"); k != nodeSettings {
		t.Errorf("settings footer row → kind %d, want nodeSettings", k)
	}
	// The top border and a trailing padding row resolve to no node.
	if got := m.NodeAtRow(0); got != -1 {
		t.Errorf("top border row → %d, want -1 (no node)", got)
	}
}

// --- Click activation: a left click does what Enter on that row does ---

// clickSidebarKind left-clicks the first sidebar row that resolves to a
// node of the given kind, returning the updated model and any command.
func clickSidebarKind(t *testing.T, m *InboxModel, kind sidebarNodeKind) (*InboxModel, tea.Cmd) {
	t.Helper()
	_ = m.View() // refresh the hit-map for the current frame
	y := -1
	for i, flatIdx := range m.sidebar.rowFlat {
		if flatIdx >= 0 && flatIdx < len(m.sidebar.flat) && m.sidebar.flat[flatIdx].Kind() == kind {
			y = i + sidebarTopBorderRows
			break
		}
	}
	if y < 0 {
		t.Fatalf("no rendered sidebar row resolves to kind %d", kind)
	}
	nm, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: y})
	return nm.(*InboxModel), cmd
}

func TestSidebarClick_SessionEmitsSelect(t *testing.T) {
	t.Parallel()
	m := newInboxWithSidebar(t)
	m, cmd := clickSidebarKind(t, m, nodeSession)
	if cmd == nil {
		t.Fatal("clicking a session produced no command")
	}
	sel, ok := cmd().(sessionSelectedFromSidebarMsg)
	if !ok {
		t.Fatalf("clicking a session emitted %T, want sessionSelectedFromSidebarMsg", cmd())
	}
	if sel.sessionID == "" {
		t.Error("session-select message carries an empty sessionID")
	}
}

func TestSidebarClick_WorktreeTogglesExpand(t *testing.T) {
	t.Parallel()
	m := newInboxWithSidebar(t)
	// The cwd worktree auto-expands, so its sessions are in the flat list.
	// Clicking it collapses them, shrinking the list.
	before := len(m.sidebar.flat)
	m, _ = clickSidebarKind(t, m, nodeWorktree)
	if after := len(m.sidebar.flat); after >= before {
		t.Errorf("clicking the expanded worktree should collapse it: flat %d -> %d", before, after)
	}
}

func TestSidebarClick_SettingsOpensSettingsScreen(t *testing.T) {
	t.Parallel()
	m := newInboxWithSidebar(t)
	m, _ = clickSidebarKind(t, m, nodeSettings)
	if m.screen != screenSettings {
		t.Errorf("clicking Settings → screen %v, want screenSettings", m.screen)
	}
}

func TestSidebarClick_AllSessionsReturnsToInbox(t *testing.T) {
	t.Parallel()
	m := newInboxWithSidebar(t)
	m.showSettings() // preview the settings page in the right pane
	if m.screen != screenSettings {
		t.Fatalf("setup: screen = %v, want screenSettings", m.screen)
	}
	m, _ = clickSidebarKind(t, m, nodeAllSessions)
	if m.screen != screenInbox {
		t.Errorf("clicking All sessions → screen %v, want screenInbox", m.screen)
	}
}

func TestSidebarClick_AllSessionsFromChatReturnsToInbox(t *testing.T) {
	t.Parallel()
	m := newInboxWithSidebar(t) // starts on the chat screen
	if m.screen != screenSession {
		t.Fatalf("setup: screen = %v, want screenSession", m.screen)
	}
	m, cmd := clickSidebarKind(t, m, nodeAllSessions)
	if cmd == nil {
		t.Fatal("clicking All sessions from a chat produced no command")
	}
	if _, ok := cmd().(backToInboxMsg); !ok {
		t.Fatalf("clicking All sessions from a chat emitted %T, want backToInboxMsg", cmd())
	}
}

// With the sidebar visible, mouse reporting is on for every screen, so a
// wheel over the sidebar must move its cursor even on the inbox screen
// (where it previously couldn't, mouse reporting being chat-only).
func TestSidebarMouse_WheelOverSidebarOnInboxScreen(t *testing.T) {
	t.Parallel()
	m := newInboxWithSidebar(t)
	m.screen = screenInbox
	_ = m.View()
	before := m.sidebar.cursor
	nm, _ := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 2, Y: 5})
	m = nm.(*InboxModel)
	if m.sidebar.cursor == before {
		t.Errorf("wheel over the sidebar on the inbox screen did not move its cursor (stuck at %d)", before)
	}
}

// A wheel over the right pane on a non-chat screen (sessionView is nil)
// must not panic — it re-dispatches as an arrow key.
func TestSidebarMouse_WheelOverRightPaneOnInboxNoPanic(t *testing.T) {
	t.Parallel()
	m := newInboxWithSidebar(t)
	m.screen = screenInbox
	m.sessionView = nil
	_ = m.View()
	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 90, Y: 10})
}

// A click that lands on the top border (no node) must not change state.
func TestSidebarClick_OnBorderIsNoOp(t *testing.T) {
	t.Parallel()
	m := newInboxWithSidebar(t)
	_ = m.View()
	screenBefore, flatBefore := m.screen, len(m.sidebar.flat)
	nm, _ := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 0}) // row 0 = top border
	m = nm.(*InboxModel)
	if m.screen != screenBefore || len(m.sidebar.flat) != flatBefore {
		t.Errorf("border click changed state: screen %v->%v, flat %d->%d",
			screenBefore, m.screen, flatBefore, len(m.sidebar.flat))
	}
}

// Enter on "All sessions" must return to the inbox list — same as a left click.
func TestSidebarKey_EnterOnAllSessionsReturnsToInbox(t *testing.T) {
	t.Parallel()
	m := newInboxWithSidebar(t)
	m.showSettings()
	if m.screen != screenSettings {
		t.Fatalf("setup: screen = %v, want screenSettings", m.screen)
	}
	m.setPane(paneSidebar)
	m.sidebar.SetCursor(0) // "All sessions" is always flat index 0
	nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = nm.(*InboxModel)
	if m.screen != screenInbox {
		t.Errorf("Enter on All sessions → screen %v, want screenInbox", m.screen)
	}
}
