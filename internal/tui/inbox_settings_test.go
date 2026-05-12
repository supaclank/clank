package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestInbox_EnterOnSettingsFooter_OpensSettingsScreen verifies the full
// wiring from sidebar footer Enter → inbox switches to the settings screen.
// This is the user-visible path and the thing most likely to break if any
// of the intermediate handlers grow additional conditions.
func TestInbox_EnterOnSettingsFooter_OpensSettingsScreen(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  120,
		height: 40,
		pane:   paneSidebar,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
			focused:    true,
			entries:    makeEntries(1),
		},
	}
	// Park cursor on the settings footer row.
	m.sidebar.cursor = m.sidebar.settingsCursorIndex()

	if m.screen != screenInbox {
		t.Fatalf("precondition: expected screenInbox, got %v", m.screen)
	}

	m.handleSidebarKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.screen != screenSettings {
		t.Errorf("expected screenSettings after Enter on settings footer, got %v", m.screen)
	}
	if m.sidebar.Focused() {
		t.Error("expected sidebar to lose focus when settings screen opens")
	}
}

// TestInbox_RightArrowOnSettingsFooter_OpensSettingsScreen mirrors the Enter
// test for the right-arrow path (alternative activation gesture).
func TestInbox_RightArrowOnSettingsFooter_OpensSettingsScreen(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  120,
		height: 40,
		pane:   paneSidebar,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
			focused:    true,
		},
	}
	m.sidebar.cursor = m.sidebar.settingsCursorIndex()

	m.handleSidebarKey(tea.KeyPressMsg{Code: tea.KeyRight})

	if m.screen != screenSettings {
		t.Errorf("expected screenSettings after right-arrow on settings footer, got %v", m.screen)
	}
}

// TestInbox_NavigatingOffSettingsRow_ClosesSettingsScreen verifies the
// UX contract: while the settings screen is showing but the sidebar has
// focus (user pressed left to return to the sidebar), scrolling off the
// settings row drops the settings preview. Previously the settings page
// stayed visible behind a hovered branch, forcing the user to jump into
// the right pane and press esc to dismiss it.
func TestInbox_NavigatingOffSettingsRow_ClosesSettingsScreen(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  120,
		height: 40,
		screen: screenSettings,
		pane:   paneSidebar,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
			focused:    true,
			entries:    makeEntries(1),
		},
		settings: newSettingsView("", "", "", ""),
	}
	// Park cursor on the settings row.
	m.sidebar.cursor = m.sidebar.settingsCursorIndex()

	// Press up to move off Settings. Cloud sits just above Settings in
	// the footer (added after this test was written), so the immediate
	// up-arrow lands on Cloud — that previews its own panel, making
	// screenCloud a valid post-move state. The contract this test
	// guards is "Settings is no longer the showing screen", not "we
	// always return to Inbox."
	m.updateSettings(tea.KeyPressMsg{Code: tea.KeyUp})

	if m.sidebar.CursorOnSettings() {
		t.Fatalf("precondition: cursor should have moved off settings row")
	}
	if m.screen == screenSettings {
		t.Errorf("expected settings to no longer be showing after cursor left settings row, got screenSettings")
	}
}

// TestInbox_HoveringSettingsRow_ShowsSettingsScreen verifies the symmetric
// counterpart to TestInbox_NavigatingOffSettingsRow_ClosesSettingsScreen:
// when the sidebar cursor lands on the ⚙ Settings footer (e.g. via j/down
// from a branch row), the right pane should auto-render the settings page
// — mirroring how hovering a worktree updates the session list. Focus must
// stay on the sidebar so the user can keep navigating with j/k.
func TestInbox_HoveringSettingsRow_ShowsSettingsScreen(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  120,
		height: 40,
		screen: screenInbox,
		pane:   paneSidebar,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
			focused:    true,
			entries:    makeEntries(1),
		},
	}
	// Start one row above settings so a single 'down' lands on it.
	m.sidebar.cursor = m.sidebar.settingsCursorIndex() - 1

	m.handleSidebarKey(tea.KeyPressMsg{Code: tea.KeyDown})

	if !m.sidebar.CursorOnSettings() {
		t.Fatalf("precondition: cursor should be on settings row")
	}
	if m.screen != screenSettings {
		t.Errorf("expected screenSettings after hovering settings row, got %v", m.screen)
	}
	if !m.sidebar.Focused() {
		t.Error("expected sidebar to retain focus on hover (no focus shift)")
	}
	if m.pane != paneSidebar {
		t.Errorf("expected pane to remain paneSidebar on hover, got %v", m.pane)
	}
}

// TestInbox_CloseSettingsReturnsToInbox verifies esc from the settings page
// lands the user back on the inbox screen with sidebar focused (parity
// with the rest of the two-pane navigation).
func TestInbox_CloseSettingsReturnsToInbox(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  120,
		height: 40,
		screen: screenSettings,
		pane:   paneSessions,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
		},
		settings: newSettingsView("", "", "", ""),
	}
	m.settings.SetFocused(true)

	m.closeSettings()

	if m.screen != screenInbox {
		t.Errorf("expected screenInbox after closeSettings, got %v", m.screen)
	}
	if !m.sidebar.Focused() {
		t.Error("expected sidebar to regain focus on close")
	}
}
