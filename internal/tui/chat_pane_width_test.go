package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/supaclank/clank/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

// A wrapped tool-summary line must stay within maxWidth and keep its dim
// styling on every line. The old path styled the whole string then wrapped it,
// which dropped the color on continuation lines (a stray bright "white" wrapped
// line) and overshot the width by a cell — both of which corrupt rendering on
// narrow panes. The status icon keeps its own color on the first line.
func TestToolLineWrapStaysWithinWidthAndStyled(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "s")
	part := agent.Part{ID: "t", Type: "tool", Tool: "Bash", Status: agent.PartFailed,
		Input: map[string]any{"command": strings.Repeat("git add some/file && ", 8)}}
	e := &displayEntry{kind: entryTool, partID: "t", toolPart: &part}
	for _, maxWidth := range []int{24, 40, 60} {
		lines := m.renderEntryUncached(e, false, false, maxWidth)
		if len(lines) < 2 {
			t.Fatalf("maxWidth=%d: expected the long tool line to wrap, got %d line(s)", maxWidth, len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > maxWidth {
				t.Errorf("maxWidth=%d line %d: width %d exceeds maxWidth", maxWidth, i, w)
			}
			if !strings.HasPrefix(l, "\x1b[") {
				t.Errorf("maxWidth=%d line %d lost its style on wrap (renders in terminal default): %q", maxWidth, i, ansi.Strip(l))
			}
		}
	}
}

// Toggling the sidebar reshapes the layout and reflows wrapped lines, which can
// strand stale cells; the toggle must request a full repaint so the terminal
// clears them. Asserts the command equals tea.ClearScreen.
func TestToggleSidebarRequestsRepaint(t *testing.T) {
	t.Parallel()
	m := &InboxModel{width: 100, height: 40, sidebarWidthRatio: defaultSidebarWidthRatio}
	m.sidebar = NewSidebarModel(nil, "h", agent.GitRef{}, t.TempDir())
	cmd := m.toggleSidebar()
	if cmd == nil || reflect.TypeOf(cmd()) != reflect.TypeOf(tea.ClearScreen()) {
		t.Fatal("toggleSidebar must return tea.ClearScreen so the terminal repaints and drops stale cells")
	}
}

// inboxFrameWidth renders the session screen and returns the rendered frame
// width (widest line, trailing padding included — what the terminal actually
// paints).
func inboxFrameWidth(t *testing.T, termW int, sidebarHidden, inputActive bool) int {
	t.Helper()
	sv := NewSessionViewModel(nil, "s")
	sv.historyLoaded = true
	sv.paneFocused = true
	sv.inputActive = inputActive
	sv.entries = append(sv.entries, displayEntry{kind: entryText, partID: "p", messageID: "m", content: strings.Repeat("the quick brown fox jumps ", 10)})
	sv.cursor = 0
	m := &InboxModel{
		width: termW, height: 40, screen: screenSession, sessionView: sv,
		sidebar:           NewSidebarModel(nil, "h", agent.GitRef{}, t.TempDir()),
		sidebarWidthRatio: defaultSidebarWidthRatio, pane: paneSessions, sidebarHidden: sidebarHidden,
	}
	m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
	frame := 0
	for _, l := range strings.Split(m.View().Content, "\n") {
		if w := ansi.StringWidth(l); w > frame {
			frame = w
		}
	}
	return frame
}

// Toggling the worktree sidebar must not change the rendered frame width, and
// the frame must never exceed the terminal. Otherwise the wider frame's extra
// columns are left stranded on screen when toggling (a ghost right border), and
// the chat can bleed past the terminal edge. Regression for an over-long help
// line that wasn't clamped to the pane width — visible mainly on narrower panes
// where the hint bar is wider than the chat column.
func TestSidebarToggleKeepsFrameWidth(t *testing.T) {
	t.Parallel()
	for termW := 80; termW <= 130; termW++ {
		for _, inputActive := range []bool{false, true} {
			two := inboxFrameWidth(t, termW, false, inputActive)
			one := inboxFrameWidth(t, termW, true, inputActive)
			if two > termW || one > termW {
				t.Fatalf("term=%d inputActive=%v: frame overflows terminal (two-pane=%d single=%d)", termW, inputActive, two, one)
			}
			if two != one {
				t.Fatalf("term=%d inputActive=%v: frame width differs by mode (two-pane=%d single=%d) → toggling strands %d cols",
					termW, inputActive, two, one, two-one)
			}
		}
	}
}

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
