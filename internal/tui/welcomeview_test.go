package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestRenderWelcomePane_ShowsActionList(t *testing.T) {
	t.Parallel()
	m := &InboxModel{width: 100, height: 40, screen: screenWelcome, pane: paneSessions}

	out := m.renderWelcomePane()

	for _, want := range []string{
		"Welcome to CLANK",
		"Start here",
		"New session",
		"New worktree session",
		"Import sessions",
		"Settings",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("welcome pane missing %q\n---\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"Get set up", "Around the TUI", "shift+n"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("welcome pane should not show shortcut cheat-sheet entry %q\n---\n%s", unwanted, out)
		}
	}
	var startHereColumn, newSessionColumn int
	for _, line := range strings.Split(ansi.Strip(out), "\n") {
		switch {
		case strings.Contains(line, "Start here"):
			startHereColumn = lipgloss.Width(line[:strings.Index(line, "Start here")])
		case strings.Contains(line, "New session"):
			newSessionColumn = lipgloss.Width(line[:strings.Index(line, "New session")])
		}
	}
	if startHereColumn != newSessionColumn-1 {
		t.Errorf("Start here starts at column %d, New session at %d; want Start here one column left", startHereColumn, newSessionColumn)
	}
}

func TestWelcomeKey_NavigatesAndActivatesSelectedAction(t *testing.T) {
	t.Parallel()
	m := &InboxModel{screen: screenWelcome, pane: paneSessions}

	_, _ = m.handleWelcomeKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.welcomeCursor != 1 {
		t.Fatalf("down → cursor %d, want 1", m.welcomeCursor)
	}
	_, _ = m.handleWelcomeKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.welcomeCursor != 0 {
		t.Fatalf("up → cursor %d, want 0", m.welcomeCursor)
	}
	_, _ = m.handleWelcomeKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.welcomeCursor != 0 {
		t.Fatalf("up at first action → cursor %d, want 0", m.welcomeCursor)
	}

	m.welcomeCursor = welcomeActionSettings
	_, _ = m.handleWelcomeKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.welcomeCursor != welcomeActionSettings {
		t.Fatalf("down at last action → cursor %d, want %d", m.welcomeCursor, welcomeActionSettings)
	}

	m.welcomeCursor = welcomeActionImportSessions
	_, _ = m.handleWelcomeKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.showImportSessions {
		t.Error("Enter on Import sessions should open the import dialog")
	}
}

func TestRenderWelcomePane_CentersContent(t *testing.T) {
	t.Parallel()
	m := &InboxModel{width: 100, height: 40, screen: screenWelcome, pane: paneSessions}

	lines := strings.Split(ansi.Strip(m.renderWelcomePane()), "\n")
	headerRow := -1
	for i, line := range lines {
		if strings.Contains(line, "Welcome to CLANK") {
			headerRow = i
			if len(line) == len(strings.TrimLeft(line, " ")) {
				t.Errorf("welcome header is not horizontally centered: %q", line)
			}
			break
		}
	}
	if headerRow <= 0 {
		t.Errorf("welcome header row = %d, want vertical padding above it", headerRow)
	}
}

func TestCenterWelcomeContent_PreservesActionAlignment(t *testing.T) {
	t.Parallel()
	content := "Start here\n› New session\n  Settings"
	lines := strings.Split(centerWelcomeContent(content, 40, 8), "\n")

	columnOf := func(line, text string) int {
		t.Helper()
		index := strings.Index(line, text)
		if index < 0 {
			t.Fatalf("%q missing from %q", text, line)
		}
		return lipgloss.Width(line[:index])
	}
	newSessionColumn := columnOf(lines[3], "New session")
	settingsColumn := columnOf(lines[4], "Settings")
	if newSessionColumn != settingsColumn {
		t.Errorf("action labels start in columns %d and %d, want the same column", newSessionColumn, settingsColumn)
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
