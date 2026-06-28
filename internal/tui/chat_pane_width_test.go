package tui

import (
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

// renderChatRightmost renders the inbox showing one long, selected agent
// message and reports the rightmost non-blank column plus the worst line that
// overflows the terminal width. The chat region is always to the right of the
// sidebar, so the rightmost column belongs to the chat.
func renderChatRightmost(t *testing.T, termW int, sidebarHidden bool) (rightmost, overflow int) {
	t.Helper()
	body := strings.Repeat("the quick brown fox jumps over the lazy dog ", 8)
	sv := NewSessionViewModel(nil, "s")
	sv.historyLoaded = true
	sv.paneFocused = true
	sv.entries = append(sv.entries, displayEntry{kind: entryText, partID: "p", messageID: "m", content: body})
	sv.cursor = 0
	m := &InboxModel{
		width: termW, height: 40, screen: screenSession, sessionView: sv,
		sidebar:           NewSidebarModel(nil, "h", agent.GitRef{}, t.TempDir()),
		sidebarWidthRatio: defaultSidebarWidthRatio, pane: paneSessions, sidebarHidden: sidebarHidden,
	}
	m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
	for _, l := range strings.Split(m.View().Content, "\n") {
		if w := ansi.StringWidth(l); w > termW && w > overflow {
			overflow = w
		}
		if r := ansi.StringWidth(strings.TrimRight(ansi.Strip(l), " ")); r > rightmost {
			rightmost = r
		}
	}
	return
}

// The chat fills to the same right edge whether the worktree sidebar is shown
// or hidden — a paneWrapBuffer gutter and nothing more — and never overflows.
// Regression for two reports: "lots of whitespace on the right" (the chat
// stopped well short of the edge) and "the gap differs with the sidebar shown"
// (the sidebar renders paneBorderInset narrower than its allocation, which made
// the chat stop short only in two-pane mode).
func TestChatFillsSameRightEdgeBothModes(t *testing.T) {
	t.Parallel()
	for _, termW := range []int{90, 120, 160, 200} {
		two, twoOver := renderChatRightmost(t, termW, false)
		one, oneOver := renderChatRightmost(t, termW, true)
		if twoOver > 0 || oneOver > 0 {
			t.Fatalf("term=%d: content overflows terminal (two-pane=%d single=%d)", termW, twoOver, oneOver)
		}
		if two != one {
			t.Errorf("term=%d: chat right edge differs by mode (two-pane=%d single=%d)", termW, two, one)
		}
		if gap := termW - two; gap != paneWrapBuffer {
			t.Errorf("term=%d: chat right gap=%d, want paneWrapBuffer=%d", termW, gap, paneWrapBuffer)
		}
	}
}

// A selected message's bordered box fills the whole chat width so both borders
// sit snug — the right border isn't pushed in by glamour's document margin.
// Guards chatMarkdownStyle (margin zeroed): with the default DarkStyle margin
// the box would be 2 columns narrower than the view.
func TestSelectedBoxFillsChatWidth(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("alpha bravo charlie delta echo foxtrot golf ", 4)
	for _, w := range []int{50, 80, 120} {
		m := NewSessionViewModel(nil, "s")
		m.width = w
		m.paneFocused = true
		e := &displayEntry{kind: entryText, partID: "a", content: long}
		boxW := 0
		for _, l := range m.renderEntry(e, true, false) {
			if lw := ansi.StringWidth(strings.TrimRight(ansi.Strip(l), " ")); lw > boxW {
				boxW = lw
			}
		}
		if boxW != w {
			t.Errorf("w=%d: selected box width=%d, want %d (box should fill the chat width, snug on both sides)", w, boxW, w)
		}
	}
}

// Selecting a message must not re-wrap its text. A navigable entry is handed
// the same content width to glamour whether or not it is the cursor, because
// the border's footprint is reserved for every navigable entry rather than
// only the selected one. Without this, moving the cursor onto a message
// visibly reflowed its body.
func TestSelectingMessageDoesNotRewrap(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("word ", 40)
	for _, w := range []int{60, 90, 120} {
		m := NewSessionViewModel(nil, "s")
		m.width = w
		m.paneFocused = true
		unsel := &displayEntry{kind: entryText, partID: "a", content: body}
		sel := &displayEntry{kind: entryText, partID: "b", content: body}
		m.renderEntry(unsel, false, false)
		m.renderEntry(sel, true, false)
		if unsel.renderedWidth != sel.renderedWidth {
			t.Errorf("w=%d: content width differs (unselected=%d selected=%d) → text re-wraps on select",
				w, unsel.renderedWidth, sel.renderedWidth)
		}
	}
}

// Regression: unselected navigable entries must not overflow the terminal.
// Each content line is wrapped at contentWidth, then "  " is prepended — the
// two must together stay within m.width. (Originally caught when maxWidth was
// widened to m.width; the border-footprint reservation keeps it safe.)
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
