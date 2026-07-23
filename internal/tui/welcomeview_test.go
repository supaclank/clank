package tui

import (
	"strings"
	"testing"
)

// TestRenderWelcomePane_ShowsOnboarding verifies the home screen renders
// the welcome / onboarding content (replacing the removed all-sessions
// list) with the key actions and orientation hints.
func TestRenderWelcomePane_ShowsOnboarding(t *testing.T) {
	t.Parallel()
	m := &InboxModel{width: 100, height: 40, screen: screenWelcome, pane: paneSessions}

	out := m.renderWelcomePane()

	for _, want := range []string{
		"Welcome to CLANK",
		"Import your existing sessions",
		"Settings",
		"Toggle the sidebar",
		"New session on a fresh worktree",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("welcome pane missing %q\n---\n%s", want, out)
		}
	}
}

// TestView_WelcomeScreenIsDefault verifies the default (zero-value) screen
// renders the welcome pane through the top-level View, and that no residual
// "Inbox" list header leaks through.
func TestView_WelcomeScreenIsDefault(t *testing.T) {
	t.Parallel()
	// sidebarHidden keeps this single-pane so View() renders only the
	// welcome pane (an unconfigured sidebar isn't the subject here).
	m := &InboxModel{width: 100, height: 40, sidebarHidden: true, sidebarWidthRatio: defaultSidebarWidthRatio}
	if m.screen != screenWelcome {
		t.Fatalf("default screen = %v, want screenWelcome (zero value)", m.screen)
	}

	view := m.View()
	content := view.Content
	if !strings.Contains(content, "Welcome to CLANK") {
		t.Errorf("default View() does not render the welcome pane\n---\n%s", content)
	}
	if strings.Contains(content, "CLANK  Inbox") {
		t.Errorf("default View() still renders the old inbox list header")
	}
}
