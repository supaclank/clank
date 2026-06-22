package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
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

func TestInboxView_MouseFollowsSidebarVisibility(t *testing.T) {
	t.Parallel()

	// Mouse reporting is enabled whenever the sidebar is visible, so its
	// rows are clickable on every screen. When the sidebar is hidden on a
	// non-chat screen there's nothing mouse-driven, so it stays OFF and
	// native terminal text-select keeps working without holding Option.
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
		t.Run(tc.name+" sidebar visible → mouse on", func(t *testing.T) {
			t.Parallel()
			m := NewInboxModel(nil)
			m.width = 120 // wide → two-pane → sidebar visible
			m.height = 40
			m.screen = tc.screen
			m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)

			if v := m.View(); v.MouseMode != tea.MouseModeCellMotion {
				t.Fatalf("MouseMode = %v on %s with the sidebar visible; want MouseModeCellMotion so sidebar rows are clickable", v.MouseMode, tc.name)
			}
		})
		t.Run(tc.name+" sidebar hidden → mouse off", func(t *testing.T) {
			t.Parallel()
			m := NewInboxModel(nil)
			m.width = 120
			m.height = 40
			m.screen = tc.screen
			m.sidebarHidden = true // no sidebar, non-chat screen → nothing mouse-driven
			m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)

			if v := m.View(); v.MouseMode == tea.MouseModeCellMotion {
				t.Fatalf("MouseMode = MouseModeCellMotion on %s with the sidebar hidden; should be off so native text-select works", tc.name)
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

// Mouse routing by hover (web-like): events go to the pane under the
// cursor, not the focused pane. A click in the sidebar focuses it but
// does not start a chat selection; a wheel over the sidebar moves the
// sidebar cursor by 1 without touching chat scrollOffset; a drag that
// crosses the boundary stays with the pane that received the press.

func newInboxWithSidebar(t *testing.T) *InboxModel {
	t.Helper()
	m := newInboxWithChat(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	// Cwd-pinned worktree auto-expands so the session row is visible
	// in the flat list without manual expansion. Gives us >=3 rows
	// (all-sessions, worktree, session) for cursor-move assertions.
	m.sidebar = newTestSidebarCwd(now, "/r/x",
		agent.SessionInfo{ID: "abc", UpdatedAt: now, GitRef: agent.GitRef{LocalPath: "/r/x"}},
		agent.SessionInfo{ID: "def", UpdatedAt: now.Add(-time.Hour), GitRef: agent.GitRef{LocalPath: "/r/x"}},
	)
	m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
	m.sidebar.cursor = 1
	_ = m.View() // compute layout so chatPaneXOffset is valid
	return m
}

func TestInboxMouse_ClickInSidebarFocusesSidebar_NoChatSelection(t *testing.T) {
	t.Parallel()

	m := newInboxWithSidebar(t)
	m.pane = paneSessions
	m.sessionView.selection.startX = -1 // sentinel: no selection started

	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 5})

	if m.pane != paneSidebar {
		t.Errorf("pane = %v after sidebar click, want paneSidebar (click should focus the clicked pane)", m.pane)
	}
	if m.sessionView.selection.startX != -1 {
		t.Errorf("chat selection started on sidebar click (startX=%d); click must not bubble to chat", m.sessionView.selection.startX)
	}
}

func TestInboxMouse_ClickInChatFocusesChat(t *testing.T) {
	t.Parallel()

	m := newInboxWithSidebar(t)
	m.pane = paneSidebar
	offset := m.chatPaneXOffset()

	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: offset + 5, Y: 5})

	if m.pane != paneSessions {
		t.Errorf("pane = %v after chat click, want paneSessions", m.pane)
	}
}

func TestInboxMouse_WheelDownInSidebarMovesCursorByOne(t *testing.T) {
	t.Parallel()

	m := newInboxWithSidebar(t)
	m.sidebar.cursor = 0
	chatOffsetBefore := m.sessionView.scrollOffset

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 0, Y: 5})

	if m.sidebar.cursor != 1 {
		t.Errorf("sidebar cursor = %d after wheel down, want 1 (wheel moves cursor by exactly 1, not wheelScrollLines)", m.sidebar.cursor)
	}
	if m.sessionView.scrollOffset != chatOffsetBefore {
		t.Errorf("chat scrollOffset changed on sidebar wheel: %d -> %d; wheel-over-sidebar must not bubble to chat", chatOffsetBefore, m.sessionView.scrollOffset)
	}
}

func TestInboxMouse_WheelUpInSidebarMovesCursorByOne(t *testing.T) {
	t.Parallel()

	m := newInboxWithSidebar(t)
	m.sidebar.cursor = 2

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 0, Y: 5})

	if m.sidebar.cursor != 1 {
		t.Errorf("sidebar cursor = %d after wheel up, want 1", m.sidebar.cursor)
	}
}

func TestInboxMouse_WheelInSidebarClampsNoWrap(t *testing.T) {
	t.Parallel()

	t.Run("wheel up at top stays at 0", func(t *testing.T) {
		t.Parallel()
		m := newInboxWithSidebar(t)
		m.sidebar.cursor = 0
		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 0, Y: 5})
		if m.sidebar.cursor != 0 {
			t.Errorf("sidebar cursor wrapped on wheel up at top: got %d, want 0 (wheel must not wrap, even though keyboard nav does)", m.sidebar.cursor)
		}
	})

	t.Run("wheel down at bottom stays at last", func(t *testing.T) {
		t.Parallel()
		m := newInboxWithSidebar(t)
		last := len(m.sidebar.flat) - 1
		m.sidebar.cursor = last
		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 0, Y: 5})
		if m.sidebar.cursor != last {
			t.Errorf("sidebar cursor wrapped on wheel down at bottom: got %d, want %d", m.sidebar.cursor, last)
		}
	})
}

func TestInboxMouse_WheelInChatScrollsChat_NotSidebar(t *testing.T) {
	t.Parallel()

	m := newInboxWithSidebar(t)
	for i := 0; i < 30; i++ {
		m.sessionView.entries = append(m.sessionView.entries, displayEntry{kind: entryText, content: "padding"})
	}
	m.sessionView.scrollOffset = 0
	m.sessionView.follow = false
	m.setPane(paneSidebar) // start with sidebar focused to verify wheel-over-chat steals focus
	_ = m.View()
	sidebarCursorBefore := m.sidebar.cursor
	offset := m.chatPaneXOffset()

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: offset + 5, Y: 10})

	if m.sessionView.scrollOffset == 0 {
		t.Error("chat did not scroll on wheel-over-chat")
	}
	if m.sidebar.cursor != sidebarCursorBefore {
		t.Errorf("sidebar cursor moved on wheel-over-chat: %d -> %d", sidebarCursorBefore, m.sidebar.cursor)
	}
	if m.pane != paneSessions {
		t.Errorf("pane = %v after wheel-over-chat, want paneSessions (wheel focuses the pane under cursor for symmetry with click)", m.pane)
	}
}

func TestInboxMouse_WheelFocusesPaneUnderCursor(t *testing.T) {
	t.Parallel()

	// Symmetric focus follow-the-cursor rule: any mouse interaction
	// with a pane focuses it. Without this, wheel-over-unfocused-
	// sidebar moves m.cursor invisibly because sidebar_render.go
	// only highlights the cursor row when m.focused.

	t.Run("wheel over sidebar focuses sidebar", func(t *testing.T) {
		t.Parallel()
		m := newInboxWithSidebar(t)
		m.setPane(paneSessions)

		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 0, Y: 5})

		if m.pane != paneSidebar {
			t.Errorf("pane = %v after wheel-over-sidebar, want paneSidebar", m.pane)
		}
		if !m.sidebar.focused {
			t.Error("sidebar.focused = false; cursor row won't render, defeating the wheel's effect")
		}
	})

	t.Run("wheel over chat focuses chat", func(t *testing.T) {
		t.Parallel()
		m := newInboxWithSidebar(t)
		m.setPane(paneSidebar)
		offset := m.chatPaneXOffset()

		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: offset + 5, Y: 5})

		if m.pane != paneSessions {
			t.Errorf("pane = %v after wheel-over-chat, want paneSessions", m.pane)
		}
	})
}

func TestInboxMouse_DragFromChatStaysInChatOnRelease(t *testing.T) {
	t.Parallel()

	// Press in chat, motion/release drift into the sidebar's X range.
	// The release must still reach the chat (selection ends there);
	// the sidebar must not see the click.
	m := newInboxWithSidebar(t)
	offset := m.chatPaneXOffset()
	paneBefore := m.pane

	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: offset + 5, Y: 10})
	if m.mouseDragOwner == nil || *m.mouseDragOwner != paneSessions {
		t.Fatalf("mouseDragOwner = %v after chat press, want &paneSessions", m.mouseDragOwner)
	}

	// Drag into sidebar X range, then release there.
	m.Update(tea.MouseMotionMsg{Button: tea.MouseLeft, X: 0, Y: 11})
	m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: 0, Y: 11})

	if m.mouseDragOwner != nil {
		t.Errorf("mouseDragOwner = %v after release, want nil", m.mouseDragOwner)
	}
	if m.pane != paneBefore {
		t.Errorf("pane changed mid-drag: %v -> %v; drag release in sidebar must not focus sidebar when press was in chat", paneBefore, m.pane)
	}
}

func TestInboxMouse_DragFromSidebarStaysInSidebarOnRelease(t *testing.T) {
	t.Parallel()

	// Press in sidebar pins the drag; release drifting into chat
	// must not start a chat selection.
	m := newInboxWithSidebar(t)
	offset := m.chatPaneXOffset()

	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 5})
	if m.mouseDragOwner == nil || *m.mouseDragOwner != paneSidebar {
		t.Fatalf("mouseDragOwner = %v after sidebar press, want &paneSidebar", m.mouseDragOwner)
	}
	chatStartXBefore := m.sessionView.selection.startX

	m.Update(tea.MouseMotionMsg{Button: tea.MouseLeft, X: offset + 5, Y: 6})
	m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: offset + 5, Y: 6})

	if m.mouseDragOwner != nil {
		t.Errorf("mouseDragOwner = %v after release, want nil", m.mouseDragOwner)
	}
	if m.sessionView.selection.startX != chatStartXBefore {
		t.Errorf("chat selection started during sidebar drag (startX %d -> %d)", chatStartXBefore, m.sessionView.selection.startX)
	}
}

func TestInboxMouse_SinglePaneRoutesEverythingToChat(t *testing.T) {
	t.Parallel()

	// Below minTwoPaneWidth or with sidebar hidden, chatPaneXOffset
	// is 0 and there is no sidebar to route to. Every event must
	// reach chat unchanged.
	m := newInboxWithSidebar(t)
	m.sidebarHidden = true
	for i := 0; i < 30; i++ {
		m.sessionView.entries = append(m.sessionView.entries, displayEntry{kind: entryText, content: "padding"})
	}
	m.sessionView.scrollOffset = 0
	m.sessionView.follow = false
	_ = m.View()

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 0, Y: 10})

	if m.sessionView.scrollOffset == 0 {
		t.Error("wheel at X=0 with sidebar hidden did not reach chat (single-pane mode must route everything to chat)")
	}
}

func TestInboxMouse_KittyWarningOverlaySwallowsMouseEvents(t *testing.T) {
	t.Parallel()

	// showKittyWarning is a blocking overlay; clicks must not fall through
	// to the sidebar while it is visible.
	m := newInboxWithSidebar(t)
	m.showKittyWarning = true
	_ = m.View()
	screenBefore := m.screen
	cursorBefore := m.sidebar.cursor

	nm, _ := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 5})
	m = nm.(*InboxModel)

	if m.screen != screenBefore || m.sidebar.cursor != cursorBefore {
		t.Errorf("mouse click fell through Kitty warning overlay: screen %v->%v, cursor %d->%d",
			screenBefore, m.screen, cursorBefore, m.sidebar.cursor)
	}
}
