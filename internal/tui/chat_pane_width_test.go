package tui

import (
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

// The chat/session view paints no outer pane border (every message draws its
// own), so it must use the full width between the sidebar and the terminal
// edge — minus only a wrap-safety gutter — rather than reserving a border's
// worth of columns. chatPaneContentWidth reclaims exactly paneBorderInset
// over the bordered sessionPaneWidth.
func TestChatPaneReclaimsBorderColumns(t *testing.T) {
	t.Parallel()
	for _, termW := range []int{90, 120, 200} {
		m := &InboxModel{width: termW, height: 40, sidebarWidthRatio: defaultSidebarWidthRatio}
		if got, want := m.chatPaneContentWidth(), m.sessionPaneWidth()+paneBorderInset; got != want {
			t.Fatalf("term=%d: chat width=%d, want sessionPaneWidth+paneBorderInset=%d", termW, got, want)
		}
	}
}

// Regression for the "lots of whitespace on the right of the chat" report:
// the rendered chat content must reach within a small gutter of the right
// edge (snug) while never overflowing the terminal width (no edge-wrap).
// Before the fix the chat left ~6-10 empty columns on the right.
func TestChatContentFillsWidthWithoutOverflow(t *testing.T) {
	t.Parallel()
	// Long body so the rendered message box actually reaches the available
	// width at every tested terminal size.
	body := strings.Repeat("the quick brown fox jumps over the lazy dog and keeps running ", 6)
	for _, termW := range []int{90, 110, 130} {
		sv := NewSessionViewModel(nil, "s")
		sv.historyLoaded = true
		sv.paneFocused = true
		sv.entries = append(sv.entries, displayEntry{kind: entryText, partID: "p", messageID: "m", content: body})
		sv.cursor = 0
		m := &InboxModel{
			width: termW, height: 40, screen: screenSession, sessionView: sv,
			sidebar:           NewSidebarModel(nil, "h", agent.GitRef{}, t.TempDir()),
			sidebarWidthRatio: defaultSidebarWidthRatio, pane: paneSessions,
		}
		m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)

		widest := 0
		for _, l := range strings.Split(m.View().Content, "\n") {
			if w := ansi.StringWidth(l); w > termW {
				t.Fatalf("term=%d: rendered line width %d overflows terminal", termW, w)
			}
			if r := ansi.StringWidth(strings.TrimRight(ansi.Strip(l), " ")); r > widest {
				widest = r
			}
		}
		// gutter = paneWrapBuffer (intentional safety) + glamour's 2-col
		// document margin; allow 1 extra column of slack.
		if gap := termW - widest; gap > paneWrapBuffer+3 {
			t.Fatalf("term=%d: chat leaves %d empty cols on the right (want <= %d)", termW, gap, paneWrapBuffer+3)
		}
	}
}

// Regression: unselected navigable entries must not overflow the terminal.
// Each content line is wrapped at contentWidth, then "  " is prepended — the
// two must together stay within m.width.
func TestUnselectedNavigableEntriesNoOverflow(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("the quick brown fox jumps over the lazy dog and keeps running ", 6)
	for _, termW := range []int{90, 110, 130} {
		sv := NewSessionViewModel(nil, "s")
		sv.historyLoaded = true
		sv.paneFocused = true
		// Two entries: cursor on the second so the first (entryText) is unselected.
		sv.entries = append(sv.entries,
			displayEntry{kind: entryText, partID: "p1", messageID: "m1", content: body},
			displayEntry{kind: entryUser, partID: "p2", messageID: "m2", content: "hi"},
		)
		sv.cursor = 1
		m := &InboxModel{
			width: termW, height: 40, screen: screenSession, sessionView: sv,
			sidebar:           NewSidebarModel(nil, "h", agent.GitRef{}, t.TempDir()),
			sidebarWidthRatio: defaultSidebarWidthRatio, pane: paneSessions,
		}
		m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)

		for _, l := range strings.Split(m.View().Content, "\n") {
			if w := ansi.StringWidth(l); w > termW {
				t.Fatalf("term=%d: unselected navigable entry overflows terminal (line width %d)", termW, w)
			}
		}
	}
}
