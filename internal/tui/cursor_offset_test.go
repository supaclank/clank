package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/acksell/clank/internal/agent"
)

// These tests pin the two coordinate-translation bugs that made a mouse
// click land below-and-left of the caret after the two-pane TUI redesign:
//
//   - Horizontal: the sidebar's rounded border renders it paneBorderInset
//     columns narrower than the requested sidebarRenderWidth(). Click
//     translation used the requested width, the caret was drawn at the
//     rendered width, so clicks landed paneBorderInset columns left.
//   - Vertical: a title carrying newlines (from the user's first prompt)
//     rendered the one-line header across multiple rows, shifting the
//     content area down while cachedHeaderRows stayed 3, so clicks
//     selected the row below.

// Horizontal, invariant: click translation must use the same origin the
// chat is actually drawn at (rendered sidebar width + gap), not the
// requested sidebar width.
func TestChatPaneXOffset_MatchesRenderedSidebarWidth(t *testing.T) {
	t.Parallel()
	m := newInboxWithChat(t)
	_ = m.View() // computes layout and caches chatPaneX

	wantOrigin := lipgloss.Width(m.sidebar.View()) + sidebarGap
	if got := m.chatPaneXOffset(); got != wantOrigin {
		t.Errorf("chatPaneXOffset()=%d, want %d (rendered sidebar width %d + gap %d); "+
			"click translation must match where the chat pane is drawn",
			got, wantOrigin, lipgloss.Width(m.sidebar.View()), sidebarGap)
	}
}

// Horizontal, behavioral: a click N columns into the chat maps to
// chat-local X=N, so the caret lands under the cursor. Clicking a few
// columns in (not on the very edge) keeps the click unambiguously inside
// the chat pane under both the old and fixed offsets, so the assertion
// turns on the translation arithmetic rather than sidebar hit-testing.
func TestChatClick_TranslatesToLocalColumn(t *testing.T) {
	t.Parallel()
	m := newInboxWithChat(t)
	_ = m.View()

	const into = 10
	chatLeftEdge := lipgloss.Width(m.sidebar.View()) + sidebarGap
	m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: chatLeftEdge + into, Y: 10})

	if got := m.sessionView.selection.startX; got != into {
		t.Errorf("click %d cols into the chat (outer X=%d) recorded chat-local startX=%d, want %d",
			into, chatLeftEdge+into, got, into)
	}
}

// Vertical, root cause: a title carrying newlines must render on a single
// header row.
func TestRenderHeader_FlattensNewlineTitle(t *testing.T) {
	t.Parallel()
	sv := newTestSessionModel(testEntries())
	sv.info = &agent.SessionInfo{Status: agent.StatusIdle, Title: "first line\nsecond line"}

	if h := lipgloss.Height(sv.renderHeader()); h != 1 {
		t.Errorf("renderHeader height=%d for a newline title, want 1 (newlines must be flattened)", h)
	}
}

// Vertical, behavioral: a newline in the title must not move the content
// area. Compares the rendered screen row of a known entry against the
// plain-title baseline — robust to entry layout, unlike asserting an
// absolute row.
func TestSessionView_NewlineTitleDoesNotShiftContent(t *testing.T) {
	t.Parallel()

	rowOfUserPrompt := func(title string) (int, *SessionViewModel) {
		sv := newTestSessionModel(testEntries())
		sv.info = &agent.SessionInfo{Status: agent.StatusIdle, Title: title}
		sv.follow = false // keep scrollOffset at 0 so entry 0 stays at the top
		sv.scrollOffset = 0
		out := sv.View().Content
		for i, line := range strings.Split(out, "\n") {
			if strings.Contains(ansi.Strip(line), "user prompt") { // entry 0's content
				return i, sv
			}
		}
		return -1, sv
	}

	plain, _ := rowOfUserPrompt("plain title")
	newline, sv := rowOfUserPrompt("first line\nsecond line")
	if plain < 0 || newline < 0 {
		t.Fatalf("entry 0 text not found in rendered output (plain row=%d, newline row=%d)", plain, newline)
	}
	if plain != newline {
		t.Errorf("newline title shifted content: \"user prompt\" at row %d vs %d for a plain title; "+
			"a newline in the title must not move the content area", newline, plain)
	}
	// And the click math agrees: the row that entry renders at maps back
	// to entry 0 through cachedHeaderRows.
	if got := sv.entryAtScreenY(newline); got != 0 {
		t.Errorf("entry 0 renders at row %d but entryAtScreenY(%d)=%d (cachedHeaderRows=%d out of sync with render)",
			newline, newline, got, sv.cachedHeaderRows)
	}
}
