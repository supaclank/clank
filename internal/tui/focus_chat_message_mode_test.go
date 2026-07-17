package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestEnterMessageMode_Idempotent locks in the helper contract: a second
// call while already in message mode must not double-bump scrollOffset.
func TestEnterMessageMode_Idempotent(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "s1")
	m.follow = false

	if cmd := m.enterMessageMode(); cmd == nil {
		t.Fatal("first enterMessageMode call should return a focus cmd")
	}
	if !m.inputActive {
		t.Fatal("inputActive should be true after first call")
	}
	firstOffset := m.scrollOffset

	if cmd := m.enterMessageMode(); cmd != nil {
		t.Error("second enterMessageMode call should be a no-op (nil cmd)")
	}
	if m.scrollOffset != firstOffset {
		t.Errorf("scrollOffset bumped twice: got %d, want %d", m.scrollOffset, firstOffset)
	}
}

// TestEnterMessageMode_FollowSkipsScrollBump covers the inverse: when the
// view is in follow mode the scroll offset must not be touched.
func TestEnterMessageMode_FollowSkipsScrollBump(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "s1")
	m.follow = true
	m.scrollOffset = 7

	m.enterMessageMode()

	if m.scrollOffset != 7 {
		t.Errorf("follow mode: scrollOffset must not change, got %d want 7", m.scrollOffset)
	}
	if !m.inputActive {
		t.Fatal("inputActive should still flip to true in follow mode")
	}
}

// TestSidebarM_FocusesActiveChatAndEntersMessageMode is the primary
// regression test for the "Enter selects, m commits" gesture. After Enter
// from the sidebar opens a session, focus stays in the sidebar (preview
// semantics). Pressing "m" then moves focus to the chat pane AND engages
// message mode so the user can immediately type.
func TestSidebarM_FocusesActiveChatAndEntersMessageMode(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:        120,
		height:       40,
		screen:       screenSession,
		pane:         paneSidebar,
		activeConnID: "s1",
	}
	m.sessionView = NewSessionViewModel(nil, "s1")
	m.sessionView.follow = true

	if _, _ = m.handleSidebarKey(tea.KeyPressMsg{Code: 'm', Text: "m"}); m.pane != paneSessions {
		t.Errorf("pane = %v, want paneSessions after sidebar 'm'", m.pane)
	}
	if !m.sessionView.inputActive {
		t.Error("inputActive = false, want true after sidebar 'm'")
	}
}

// TestSidebarM_NoActiveSession_FallsBackToMerge ensures the "m" disambiguation
// doesn't break the existing branch-merge gesture: when no session is active
// in the right pane, "m" must keep its original merge semantics. The merge
// path requires a non-default branch under the cursor; without one it's a
// no-op. The key assertion here is that the message-mode path is NOT taken
// (we'd otherwise have flipped m.pane to paneSessions).
func TestSidebarM_NoActiveSession_FallsBackToMerge(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  120,
		height: 40,
		screen: screenWelcome, // no session shown
		pane:   paneSidebar,
	}

	_, _ = m.handleSidebarKey(tea.KeyPressMsg{Code: 'm', Text: "m"})

	if m.pane != paneSidebar {
		t.Errorf("pane = %v, want paneSidebar — message-mode path should not engage without an active session", m.pane)
	}
}

// TestFocusActiveChatMessageMode_NoSession is a defensive contract test: the
// helper must be safe to call when no session view exists (e.g. user spams
// "m" on the inbox before opening anything). Returning nil + leaving state
// untouched is the documented contract.
func TestFocusActiveChatMessageMode_NoSession(t *testing.T) {
	t.Parallel()

	m := &InboxModel{screen: screenWelcome, pane: paneSidebar}

	if cmd := m.focusActiveChatMessageMode(); cmd != nil {
		t.Errorf("expected nil cmd when no session view present, got %T", cmd)
	}
	if m.pane != paneSidebar {
		t.Errorf("pane mutated despite no-op: got %v", m.pane)
	}
}
