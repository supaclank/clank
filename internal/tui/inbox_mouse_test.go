package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Mouse reporting is a single terminal-level toggle: it must be set
// on the rendered outer tea.View, otherwise the terminal translates
// wheel events into arrow keys and clicks never reach the program.
// These tests pin that wiring so a future View() refactor can't
// silently regress wheel-scroll and click-to-select in the chat.

func newInboxWithChat(t *testing.T) *InboxModel {
	t.Helper()
	m := NewInboxModel(nil)
	m.width = 120
	m.height = 40
	m.screen = screenSession
	m.sessionView = NewSessionViewModel(nil, "test-session")
	m.sessionView.width = m.sessionPaneWidth()
	m.sessionView.height = 30
	m.sessionView.entries = testEntries()
	m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
	return m
}

func TestInboxView_EnablesMouseOnSessionScreen(t *testing.T) {
	t.Parallel()

	t.Run("sidebar visible", func(t *testing.T) {
		t.Parallel()
		m := newInboxWithChat(t)
		m.sidebarHidden = false

		v := m.View()
		if v.MouseMode != tea.MouseModeCellMotion {
			t.Fatalf("MouseMode = %v, want MouseModeCellMotion; without it, the terminal never reports wheel/click events to the chat and the user-visible bug is wheel-scroll moves cursor one step per tick", v.MouseMode)
		}
	})

	t.Run("sidebar hidden", func(t *testing.T) {
		t.Parallel()
		m := newInboxWithChat(t)
		m.sidebarHidden = true

		v := m.View()
		if v.MouseMode != tea.MouseModeCellMotion {
			t.Fatalf("MouseMode = %v, want MouseModeCellMotion (sidebar hidden); mouse reporting must stay on regardless of sidebar visibility", v.MouseMode)
		}
	})
}

func TestInboxView_NoMouseOnNonSessionScreens(t *testing.T) {
	t.Parallel()

	// The inbox list / settings / cloud screens don't handle mouse,
	// so mouse reporting should stay OFF on those screens — that
	// keeps native terminal text-select (cmd+drag) working without
	// the user having to hold Option.
	cases := []struct {
		name   string
		screen inboxScreen
	}{
		{"inbox list", screenInbox},
		{"settings", screenSettings},
		{"cloud", screenCloud},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewInboxModel(nil)
			m.width = 120
			m.height = 40
			m.screen = tc.screen
			m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)

			v := m.View()
			if v.MouseMode == tea.MouseModeCellMotion {
				t.Fatalf("MouseMode = MouseModeCellMotion on %s screen; reserve it for screens that handle mouse so other screens keep native text-select", tc.name)
			}
		})
	}
}

func TestInboxWheel_ScrollsViewportWithoutMovingCursor(t *testing.T) {
	t.Parallel()

	// Regression for the post-redesign bug where mouse mode was off
	// on the outer view, so the terminal translated wheel events
	// into arrow keys and the chat moved its cursor one step per
	// tick. With mouse mode on, wheel events flow as MouseWheelMsg
	// through InboxModel.Update -> SessionViewModel.Update, and
	// only scrollOffset moves.

	t.Run("wheel down advances scrollOffset, leaves cursor", func(t *testing.T) {
		t.Parallel()
		m := newInboxWithChat(t)
		for i := 0; i < 30; i++ {
			m.sessionView.entries = append(m.sessionView.entries, displayEntry{kind: entryText, content: "padding"})
		}
		m.sessionView.cursor = 2
		m.sessionView.scrollOffset = 0
		m.sessionView.follow = false
		// Prime the line cache so clampScroll knows the max offset.
		_ = m.View()
		cursorBefore := m.sessionView.cursor

		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 50, Y: 10})

		if m.sessionView.scrollOffset == 0 {
			t.Errorf("scrollOffset did not advance on wheel down; got %d (event was not delivered to chat)", m.sessionView.scrollOffset)
		}
		if m.sessionView.cursor != cursorBefore {
			t.Errorf("cursor moved on wheel down: %d -> %d; wheel should not move cursor (this is the symptom of mouse mode being off)", cursorBefore, m.sessionView.cursor)
		}
	})

	t.Run("wheel up decreases scrollOffset, leaves cursor", func(t *testing.T) {
		t.Parallel()
		m := newInboxWithChat(t)
		for i := 0; i < 30; i++ {
			m.sessionView.entries = append(m.sessionView.entries, displayEntry{kind: entryText, content: "padding"})
		}
		m.sessionView.cursor = 5
		m.sessionView.scrollOffset = 10
		m.sessionView.follow = false
		_ = m.View()
		cursorBefore := m.sessionView.cursor
		offsetBefore := m.sessionView.scrollOffset

		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 50, Y: 10})

		if m.sessionView.scrollOffset >= offsetBefore {
			t.Errorf("scrollOffset did not decrease on wheel up: %d -> %d", offsetBefore, m.sessionView.scrollOffset)
		}
		if m.sessionView.cursor != cursorBefore {
			t.Errorf("cursor moved on wheel up: %d -> %d", cursorBefore, m.sessionView.cursor)
		}
	})
}

func TestInboxWheel_SidebarHiddenRoutesToChat(t *testing.T) {
	t.Parallel()

	// When the sidebar is hidden, chatPaneXOffset() is 0 and the X
	// translation is a no-op. A wheel event at X=0 should still
	// reach the chat — pins the edge case where wrong offset math
	// could drop events.

	m := newInboxWithChat(t)
	m.sidebarHidden = true
	for i := 0; i < 30; i++ {
		m.sessionView.entries = append(m.sessionView.entries, displayEntry{kind: entryText, content: "padding"})
	}
	m.sessionView.scrollOffset = 0
	m.sessionView.follow = false
	_ = m.View()

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 0, Y: 10})

	if m.sessionView.scrollOffset == 0 {
		t.Error("wheel event at X=0 with sidebar hidden was not routed to chat")
	}
}

func TestInboxWheel_XTranslatedIntoChatFrame(t *testing.T) {
	t.Parallel()

	// Mouse events arrive in the outer terminal frame. The chat is
	// shifted right by the sidebar width; the inbox must translate
	// X into the chat's local frame so click hit-tests target the
	// right column. Easiest way to assert: send a click at the
	// chat's leftmost outer X (sidebarWidth + gap) and confirm the
	// chat sees X=0.
	//
	// We can't peek inside the chat's click handler cheaply, so
	// instead we forward a MouseClickMsg whose X corresponds to the
	// chat's column 0 and verify the chat's selection.startX is 0
	// (selection records the raw X passed to Start).

	m := newInboxWithChat(t)
	_ = m.View() // ensure layout is computed
	offset := m.chatPaneXOffset()
	if offset == 0 {
		t.Fatal("test setup: expected non-zero chat pane X offset (sidebar should be visible)")
	}

	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: offset, Y: 10})

	if got := m.sessionView.selection.startX; got != 0 {
		t.Errorf("chat saw click at X=%d, want X=0 (mouse X was not translated from outer frame to chat frame; offset was %d)", got, offset)
	}
}
