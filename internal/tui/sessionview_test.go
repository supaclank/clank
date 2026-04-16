package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// TestSetEventChannel_SkipsSubscribe verifies that when SetEventChannel is
// called before Init(), the model reads from the pre-connected channel instead
// of calling subscribeEvents (which would create a second SSE connection).
// This is a regression test for the race condition where CreateSession emits
// events before the TUI subscribes.
func TestSetEventChannel_SkipsSubscribe(t *testing.T) {
	t.Parallel()

	ch := make(chan agent.Event, 16)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	model := NewSessionViewModel(nil, "test-session-123")
	model.SetEventChannel(ch, cancel)

	// Verify the channel was stored.
	if model.eventsCh == nil {
		t.Fatal("eventsCh should be set after SetEventChannel")
	}

	// Init should return commands. We can't easily introspect tea.Cmd closures,
	// but we can verify the model processes events from the pre-set channel.
	// Send an event through the channel.
	ch <- agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "test-session-123",
		Data: agent.StatusChangeData{
			OldStatus: agent.StatusStarting,
			NewStatus: agent.StatusBusy,
		},
	}
	ch <- agent.Event{
		Type:      agent.EventPartUpdate,
		SessionID: "test-session-123",
		Data: agent.PartUpdateData{
			MessageID: "msg-1",
			Part: agent.Part{
				ID:   "part-1",
				Type: "text",
				Text: "Hello from agent",
			},
		},
	}
	ch <- agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "test-session-123",
		Data: agent.StatusChangeData{
			OldStatus: agent.StatusBusy,
			NewStatus: agent.StatusIdle,
		},
	}

	// Process events directly via handleEvent (same as the integration test).
	drainTimeout := time.After(2 * time.Second)
	processed := 0
	for processed < 3 {
		select {
		case evt := <-ch:
			model.handleEvent(evt)
			processed++
		case <-drainTimeout:
			t.Fatalf("timed out, processed %d of 3 events", processed)
		}
	}

	// Verify entries were created.
	var foundText bool
	var foundStatus int
	for _, e := range model.entries {
		if e.kind == entryText && e.content == "Hello from agent" {
			foundText = true
		}
		if e.kind == entryStatus {
			foundStatus++
		}
	}

	if !foundText {
		t.Error("expected agent text entry with 'Hello from agent'")
	}
	if foundStatus < 2 {
		t.Errorf("expected at least 2 status entries, got %d", foundStatus)
	}
}

// TestTruncateStr_NarrowWidth is a regression test for the panic caused by
// negative slice bounds in truncateStr when the terminal is very narrow
// (m.width - 50 becomes negative in inbox.go renderRow).
func TestTruncateStr_NarrowWidth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{name: "negative n", s: "hello world", n: -17, want: ""},
		{name: "zero n", s: "hello world", n: 0, want: ""},
		{name: "n=1", s: "hello world", n: 1, want: "h"},
		{name: "n=2", s: "hello world", n: 2, want: "he"},
		{name: "n=3", s: "hello world", n: 3, want: "hel"},
		{name: "n=4 truncates with ellipsis", s: "hello world", n: 4, want: "h..."},
		{name: "n=len(s) no truncation", s: "hello", n: 5, want: "hello"},
		{name: "n>len(s) no truncation", s: "hello", n: 100, want: "hello"},
		{name: "normal truncation", s: "hello world", n: 8, want: "hello..."},
		{name: "empty string", s: "", n: 5, want: ""},
		{name: "empty string negative n", s: "", n: -1, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateStr(tt.s, tt.n)
			if got != tt.want {
				t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
			}
		})
	}
}

// TestIsNavigable verifies which entry kinds the cursor can land on.
func TestIsNavigable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind entryKind
		want bool
	}{
		{entryUser, true},
		{entryText, true},
		{entryThink, true},
		{entryError, true},
		{entryPermResult, true},
		{entryTool, false},
		{entryStatus, false},
	}

	for _, tt := range tests {
		if got := isNavigable(tt.kind); got != tt.want {
			t.Errorf("isNavigable(%d) = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

// newTestSessionModel creates a SessionViewModel with preset entries for testing
// cursor navigation. Does not require a daemon client.
func newTestSessionModel(entries []displayEntry) *SessionViewModel {
	m := NewSessionViewModel(nil, "test-session")
	m.entries = entries
	m.width = 80
	m.height = 40
	return m
}

// testEntries returns a mixed sequence of entries for cursor navigation tests.
// Indices and kinds:
//
//	0: entryUser    (navigable)
//	1: entryStatus  (skip)
//	2: entryText    (navigable)
//	3: entryTool    (skip)
//	4: entryTool    (skip)
//	5: entryThink   (navigable)
//	6: entryError   (navigable)
//	7: entryStatus  (skip)
//	8: entryUser    (navigable)
//	9: entryText    (navigable)
func testEntries() []displayEntry {
	return []displayEntry{
		{kind: entryUser, content: "user prompt"},
		{kind: entryStatus, content: "starting -> busy"},
		{kind: entryText, content: "agent response"},
		{kind: entryTool, content: "[read_file] foo.go"},
		{kind: entryTool, content: "[write_file] bar.go"},
		{kind: entryThink, content: "thinking about it"},
		{kind: entryError, content: "something failed"},
		{kind: entryStatus, content: "busy -> idle"},
		{kind: entryUser, content: "follow-up message"},
		{kind: entryText, content: "agent follow-up"},
	}
}

func TestCursorNavigation(t *testing.T) {
	t.Parallel()

	t.Run("next navigable entry skips tools and status", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.cursor = 0 // entryUser

		// Next from 0 should land on 2 (entryText), skipping 1 (entryStatus).
		idx := m.nextNavigableEntry(m.cursor)
		if idx != 2 {
			t.Errorf("nextNavigableEntry(0) = %d, want 2", idx)
		}

		// Next from 2 should land on 5 (entryThink), skipping 3,4 (entryTool).
		idx = m.nextNavigableEntry(2)
		if idx != 5 {
			t.Errorf("nextNavigableEntry(2) = %d, want 5", idx)
		}
	})

	t.Run("prev navigable entry skips tools and status", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.cursor = 9 // entryText (last)

		idx := m.prevNavigableEntry(m.cursor)
		if idx != 8 {
			t.Errorf("prevNavigableEntry(9) = %d, want 8", idx)
		}

		// Prev from 5 (entryThink) should land on 2 (entryText), skipping tools.
		idx = m.prevNavigableEntry(5)
		if idx != 2 {
			t.Errorf("prevNavigableEntry(5) = %d, want 2", idx)
		}
	})

	t.Run("next navigable from last navigable returns -1", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		idx := m.nextNavigableEntry(9) // 9 is last entry, also navigable
		if idx != -1 {
			t.Errorf("nextNavigableEntry(9) = %d, want -1", idx)
		}
	})

	t.Run("prev navigable from first navigable returns -1", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		idx := m.prevNavigableEntry(0)
		if idx != -1 {
			t.Errorf("prevNavigableEntry(0) = %d, want -1", idx)
		}
	})

	t.Run("next user entry skips non-user navigable entries", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		// User entries are at 0 and 8.
		idx := m.nextUserEntry(0)
		if idx != 8 {
			t.Errorf("nextUserEntry(0) = %d, want 8", idx)
		}
	})

	t.Run("prev user entry skips non-user navigable entries", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		idx := m.prevUserEntry(9)
		if idx != 8 {
			t.Errorf("prevUserEntry(9) = %d, want 8", idx)
		}
		idx = m.prevUserEntry(8)
		if idx != 0 {
			t.Errorf("prevUserEntry(8) = %d, want 0", idx)
		}
	})

	t.Run("firstNavigableEntry and lastNavigableEntry", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		if got := m.firstNavigableEntry(); got != 0 {
			t.Errorf("firstNavigableEntry() = %d, want 0", got)
		}
		if got := m.lastNavigableEntry(); got != 9 {
			t.Errorf("lastNavigableEntry() = %d, want 9", got)
		}
	})

	t.Run("firstNavigableEntry with leading non-navigable", func(t *testing.T) {
		t.Parallel()
		entries := []displayEntry{
			{kind: entryStatus, content: "starting"},
			{kind: entryTool, content: "[read]"},
			{kind: entryText, content: "hello"},
		}
		m := newTestSessionModel(entries)
		if got := m.firstNavigableEntry(); got != 2 {
			t.Errorf("firstNavigableEntry() = %d, want 2", got)
		}
	})

	t.Run("full cursor walk forward visits all navigable entries in order", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		// Expected navigable indices: 0, 2, 5, 6, 8, 9
		expected := []int{0, 2, 5, 6, 8, 9}

		visited := []int{m.firstNavigableEntry()}
		cur := visited[0]
		for {
			next := m.nextNavigableEntry(cur)
			if next == -1 {
				break
			}
			visited = append(visited, next)
			cur = next
		}

		if len(visited) != len(expected) {
			t.Fatalf("visited %v, want %v", visited, expected)
		}
		for i, v := range visited {
			if v != expected[i] {
				t.Errorf("visited[%d] = %d, want %d", i, v, expected[i])
			}
		}
	})

	t.Run("entry-to-line mapping is populated by buildContentLines", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		lines := m.buildContentLines()

		if len(m.entryStartLine) != len(m.entries) {
			t.Fatalf("entryStartLine length = %d, want %d", len(m.entryStartLine), len(m.entries))
		}
		if len(m.entryEndLine) != len(m.entries) {
			t.Fatalf("entryEndLine length = %d, want %d", len(m.entryEndLine), len(m.entries))
		}

		// Every entry should have startLine < endLine.
		for i := range m.entries {
			if m.entryStartLine[i] >= m.entryEndLine[i] {
				t.Errorf("entry %d: startLine=%d >= endLine=%d", i, m.entryStartLine[i], m.entryEndLine[i])
			}
		}

		// Last entry's endLine should equal len(lines).
		last := len(m.entries) - 1
		if m.entryEndLine[last] != len(lines) {
			t.Errorf("last entry endLine=%d, len(lines)=%d", m.entryEndLine[last], len(lines))
		}
	})

	t.Run("shift+down past last user message jumps to last navigable and follows", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		// User entries are at 0 and 8. Last navigable is 9 (entryText).
		m.cursor = 8 // last user entry
		m.follow = false

		// nextUserEntry from 8 should return -1 (no more user messages).
		idx := m.nextUserEntry(m.cursor)
		if idx != -1 {
			t.Fatalf("nextUserEntry(8) = %d, want -1", idx)
		}

		// The shift+down handler should fall through to lastNavigableEntry + follow.
		// Simulate the handler logic:
		if idx := m.nextUserEntry(m.cursor); idx >= 0 {
			m.follow = false
			m.cursor = idx
		} else {
			m.follow = true
			m.cursor = m.lastNavigableEntry()
		}

		if m.cursor != 9 {
			t.Errorf("cursor = %d, want 9", m.cursor)
		}
		if !m.follow {
			t.Error("follow should be true after jumping past last user message")
		}
	})

	t.Run("down at last navigable entry enables follow mode", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		// Last navigable entry is 9 (entryText).
		m.cursor = 9
		m.follow = false

		// nextNavigableEntry from 9 should return -1 (already at bottom).
		if idx := m.nextNavigableEntry(m.cursor); idx != -1 {
			t.Fatalf("nextNavigableEntry(9) = %d, want -1", idx)
		}

		// Simulate the down handler logic:
		if idx := m.nextNavigableEntry(m.cursor); idx >= 0 {
			m.follow = false
			m.cursor = idx
			m.cursorMoved = true
		} else {
			// Already at last navigable entry — enable follow.
			m.follow = true
			m.scrollToBottom()
		}

		if m.cursor != 9 {
			t.Errorf("cursor = %d, want 9 (should not change)", m.cursor)
		}
		if !m.follow {
			t.Error("follow should be true after pressing down at last navigable entry")
		}
	})

	t.Run("scrollToCursor positions entry near top of viewport", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		// Build content lines so entryStartLine is populated.
		m.buildContentLines()

		// Move cursor to a navigable entry that isn't the first.
		m.cursor = 5 // entryThink
		m.scrollToCursor()

		// The entry's start line should be within topMargin (2) of scrollOffset.
		startLine := m.entryStartLine[m.cursor]
		expectedOffset := startLine - 2
		if expectedOffset < 0 {
			expectedOffset = 0
		}
		if m.scrollOffset != expectedOffset {
			t.Errorf("scrollOffset = %d, want %d (entryStartLine=%d)", m.scrollOffset, expectedOffset, startLine)
		}
	})

	t.Run("scrollToCursor clamps to zero for early entries", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.buildContentLines()

		// First entry starts at line 0; scrollOffset should clamp to 0.
		m.cursor = 0
		m.scrollToCursor()

		if m.scrollOffset != 0 {
			t.Errorf("scrollOffset = %d, want 0", m.scrollOffset)
		}
	})

	t.Run("mouse scroll does not affect cursor", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.buildContentLines()
		m.cursor = 5 // some navigable entry
		m.follow = false

		// Simulate mouse wheel down via the Update handler logic.
		origCursor := m.cursor
		m.scrollOffset += 3
		m.clampScroll()

		if m.cursor != origCursor {
			t.Errorf("cursor changed from %d to %d after mouse scroll", origCursor, m.cursor)
		}
	})

	t.Run("mouse scroll offset survives View render", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.follow = false
		m.cursor = 0
		m.cursorMoved = false

		// First render to populate entryStartLine and settle scrollOffset.
		m.buildContentLines()

		// Simulate mouse wheel scrolling: adjust scrollOffset without setting cursorMoved.
		m.scrollOffset = 5
		m.clampScroll()

		// Rebuild content lines (as View() would).
		contentLines := m.buildContentLines()
		ch := m.contentHeight()

		// Simulate the View() scroll logic: cursorMoved is false, follow is false,
		// so scrollOffset should NOT be overridden.
		if m.follow {
			t.Fatal("follow should be false")
		}
		if m.cursorMoved {
			t.Fatal("cursorMoved should be false")
		}
		// Neither branch fires — scrollOffset stays at 5 (or clamped max).
		maxOffset := len(contentLines) - ch
		if maxOffset < 0 {
			maxOffset = 0
		}
		want := 5
		if want > maxOffset {
			want = maxOffset
		}
		if m.scrollOffset != want {
			t.Errorf("scrollOffset = %d, want %d (maxOffset=%d)", m.scrollOffset, want, maxOffset)
		}
	})

	t.Run("cursorMoved triggers scrollToCursor in View logic", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.follow = false
		m.scrollOffset = 0

		// Simulate keyboard navigation: move cursor and set cursorMoved.
		m.cursor = 8 // entryUser (second user message, further down)
		m.cursorMoved = true

		// Rebuild content lines as View() would.
		m.buildContentLines()

		// Simulate the View() scroll logic.
		if m.cursorMoved {
			m.scrollToCursor()
			m.cursorMoved = false
		}

		// scrollOffset should have moved to position entry 8 near top.
		startLine := m.entryStartLine[8]
		expectedOffset := startLine - 2
		if expectedOffset < 0 {
			expectedOffset = 0
		}
		if m.scrollOffset != expectedOffset {
			t.Errorf("scrollOffset = %d, want %d (entryStartLine[8]=%d)", m.scrollOffset, expectedOffset, startLine)
		}
		if m.cursorMoved {
			t.Error("cursorMoved should be reset to false after scrollToCursor")
		}
	})
}

// TestInputToggle_ScrollOffset is a regression test verifying that toggling the
// input prompt shifts the viewport content rather than overlaying it.
// Bug: when follow=false, pressing 'm' shrinks contentHeight by
// inputReservedLines but scrollOffset was left unchanged, hiding the bottom
// lines of content behind the prompt.
func TestInputToggle_ScrollOffset(t *testing.T) {
	t.Parallel()

	t.Run("opening input shifts scrollOffset up when follow is false", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.follow = false
		m.scrollOffset = 10

		m.handleKey(tea.KeyPressMsg{Code: 'm'})

		if !m.inputActive {
			t.Fatal("expected inputActive=true after pressing m")
		}
		want := 10 + inputReservedLines
		if m.scrollOffset != want {
			t.Errorf("scrollOffset = %d, want %d", m.scrollOffset, want)
		}
	})

	t.Run("opening input does not shift scrollOffset when follow is true", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.follow = true
		m.scrollOffset = 10

		m.handleKey(tea.KeyPressMsg{Code: 'm'})

		if !m.inputActive {
			t.Fatal("expected inputActive=true after pressing m")
		}
		// follow mode recalculates scrollOffset in View(), so handleKey
		// should not touch it.
		if m.scrollOffset != 10 {
			t.Errorf("scrollOffset = %d, want 10 (unchanged)", m.scrollOffset)
		}
	})

	t.Run("closing input shifts scrollOffset back down when follow is false", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.follow = false
		m.inputActive = true
		m.scrollOffset = 10 + inputReservedLines // as if opened via 'm'

		m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})

		if m.inputActive {
			t.Fatal("expected inputActive=false after pressing esc")
		}
		if m.scrollOffset != 10 {
			t.Errorf("scrollOffset = %d, want 10", m.scrollOffset)
		}
	})

	t.Run("closing input clamps scrollOffset to zero", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.follow = false
		m.inputActive = true
		m.scrollOffset = 2 // less than inputReservedLines

		m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})

		if m.inputActive {
			t.Fatal("expected inputActive=false after pressing esc")
		}
		if m.scrollOffset != 0 {
			t.Errorf("scrollOffset = %d, want 0 (clamped)", m.scrollOffset)
		}
	})

	t.Run("open then close round-trips scrollOffset", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.follow = false
		original := 7
		m.scrollOffset = original

		// Open input.
		m.handleKey(tea.KeyPressMsg{Code: 'm'})
		if m.scrollOffset != original+inputReservedLines {
			t.Fatalf("after open: scrollOffset = %d, want %d", m.scrollOffset, original+inputReservedLines)
		}

		// Close input.
		m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
		if m.scrollOffset != original {
			t.Errorf("after close: scrollOffset = %d, want %d (original)", m.scrollOffset, original)
		}
	})

	t.Run("RestoreDraft shifts scrollOffset when follow is false", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.follow = false
		m.scrollOffset = 5

		m.RestoreDraft("some draft text")

		if !m.inputActive {
			t.Fatal("expected inputActive=true after RestoreDraft")
		}
		want := 5 + inputReservedLines
		if m.scrollOffset != want {
			t.Errorf("scrollOffset = %d, want %d", m.scrollOffset, want)
		}
	})
}

// TestMouseScrollDown_ReenablesFollow verifies that scrolling down to the
// bottom of content with the mouse wheel re-enables follow mode, so that
// new messages auto-scroll and the input prompt layout works correctly.
func TestMouseScrollDown_ReenablesFollow(t *testing.T) {
	t.Parallel()

	t.Run("scroll to bottom re-enables follow", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.follow = false
		// Put scrollOffset near the bottom so one scroll-down step reaches it.
		lines := m.buildContentLines()
		maxOffset := len(lines) - m.contentHeight()
		if maxOffset < 3 {
			// Content is too short for the test to be meaningful; set a
			// large enough scrollOffset to trigger clamping to maxOffset.
			m.scrollOffset = 0
		} else {
			m.scrollOffset = maxOffset - 2 // close to bottom, within one scroll step (3 lines)
		}

		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})

		if !m.follow {
			t.Error("expected follow=true after scrolling to bottom")
		}
	})

	t.Run("scroll not at bottom keeps follow false", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		// Ensure enough content to scroll through.
		for i := 0; i < 20; i++ {
			m.entries = append(m.entries, displayEntry{kind: entryText, content: "padding line"})
		}
		m.follow = false
		m.scrollOffset = 0

		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})

		if m.follow {
			t.Error("expected follow=false when not at bottom")
		}
	})
}

// TestCtrlC_CancelsWhenBusy verifies that ctrl+c aborts the session when
// the agent is busy, setting the aborting flag and appending a "Cancelling..."
// entry. When idle, ctrl+c uses double-tap to quit.
func TestCtrlC_CancelsWhenBusy(t *testing.T) {
	t.Parallel()
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}

	t.Run("normal mode busy sets aborting state", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.info = &agent.SessionInfo{Status: agent.StatusBusy}
		entriesBefore := len(m.entries)

		_, cmd := m.handleKey(ctrlC)

		if cmd == nil {
			t.Fatal("expected a command from ctrl+c when busy")
		}
		if !m.aborting {
			t.Error("expected aborting=true")
		}
		if m.abortEntryIdx != entriesBefore {
			t.Errorf("abortEntryIdx = %d, want %d", m.abortEntryIdx, entriesBefore)
		}
		if len(m.entries) != entriesBefore+1 {
			t.Fatalf("expected %d entries, got %d", entriesBefore+1, len(m.entries))
		}
		if m.entries[m.abortEntryIdx].content != "Cancelling..." {
			t.Errorf("abort entry content = %q, want %q", m.entries[m.abortEntryIdx].content, "Cancelling...")
		}
	})

	t.Run("normal mode starting sets aborting state", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.info = &agent.SessionInfo{Status: agent.StatusStarting}

		_, cmd := m.handleKey(ctrlC)

		if cmd == nil {
			t.Fatal("expected a command from ctrl+c when starting")
		}
		if !m.aborting {
			t.Error("expected aborting=true")
		}
	})

	t.Run("normal mode idle uses double-tap", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.info = &agent.SessionInfo{Status: agent.StatusIdle}

		// First press: no quit, sets lastCtrlC.
		_, cmd := m.handleKey(ctrlC)
		if cmd == nil {
			t.Fatal("expected a command (hint timer) from first ctrl+c")
		}
		if m.lastCtrlC.IsZero() {
			t.Fatal("expected lastCtrlC to be set after first ctrl+c")
		}

		// Second press within window: quits.
		_, cmd = m.handleKey(ctrlC)
		if cmd == nil {
			t.Fatal("expected a command from second ctrl+c")
		}
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Fatalf("expected tea.QuitMsg on double-tap, got %T", msg)
		}
	})

	t.Run("normal mode nil info uses double-tap", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.info = nil

		// First press: no quit.
		_, _ = m.handleKey(ctrlC)
		if m.lastCtrlC.IsZero() {
			t.Fatal("expected lastCtrlC to be set")
		}

		// Second press: quits.
		_, cmd := m.handleKey(ctrlC)
		if cmd == nil {
			t.Fatal("expected a command from second ctrl+c")
		}
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Fatalf("expected tea.QuitMsg on double-tap, got %T", msg)
		}
	})

	t.Run("input mode busy sets aborting state", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.info = &agent.SessionInfo{Status: agent.StatusBusy}
		m.inputActive = true

		_, cmd := m.handleKey(ctrlC)

		if cmd == nil {
			t.Fatal("expected a command from ctrl+c in input mode when busy")
		}
		if !m.aborting {
			t.Error("expected aborting=true in input mode")
		}
	})

	t.Run("input mode idle uses double-tap", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(testEntries())
		m.info = &agent.SessionInfo{Status: agent.StatusIdle}
		m.inputActive = true

		// First press.
		_, _ = m.handleKey(ctrlC)
		if m.lastCtrlC.IsZero() {
			t.Fatal("expected lastCtrlC to be set")
		}

		// Second press: quits.
		_, cmd := m.handleKey(ctrlC)
		if cmd == nil {
			t.Fatal("expected a command from second ctrl+c")
		}
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Fatalf("expected tea.QuitMsg on double-tap, got %T", msg)
		}
	})
}

// TestAbortSuppressesEvents verifies that status change and error events
// are suppressed during an active abort, and the "Cancelling..." entry is
// updated to "Cancelled" in place.
func TestAbortSuppressesEvents(t *testing.T) {
	t.Parallel()

	t.Run("status change during abort updates entry in place", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{Status: agent.StatusBusy}
		m.aborting = true
		m.abortEntryIdx = len(m.entries)
		m.entries = append(m.entries, displayEntry{
			kind:    entryStatus,
			content: "Cancelling...",
		})
		entriesAfterAbort := len(m.entries)

		// Simulate busy -> idle status change.
		m.handleStatusChange(agent.StatusChangeData{
			OldStatus: agent.StatusBusy,
			NewStatus: agent.StatusIdle,
		})

		// Should NOT have appended a new entry.
		if len(m.entries) != entriesAfterAbort {
			t.Errorf("expected %d entries (no new ones), got %d", entriesAfterAbort, len(m.entries))
		}
		// Should have updated existing entry.
		if m.entries[m.abortEntryIdx].content != "Cancelled" {
			t.Errorf("abort entry = %q, want %q", m.entries[m.abortEntryIdx].content, "Cancelled")
		}
		if m.aborting {
			t.Error("expected aborting=false after terminal status")
		}
	})

	t.Run("intermediate status change during abort is suppressed", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{Status: agent.StatusBusy}
		m.aborting = true
		m.abortEntryIdx = 0
		m.entries = []displayEntry{{kind: entryStatus, content: "Cancelling..."}}

		// Simulate busy -> busy (intermediate, should be suppressed).
		m.handleStatusChange(agent.StatusChangeData{
			OldStatus: agent.StatusBusy,
			NewStatus: agent.StatusBusy,
		})

		if len(m.entries) != 1 {
			t.Errorf("expected 1 entry, got %d", len(m.entries))
		}
		if m.entries[0].content != "Cancelling..." {
			t.Errorf("entry should still be 'Cancelling...', got %q", m.entries[0].content)
		}
		if !m.aborting {
			t.Error("should still be aborting during intermediate status")
		}
	})

	t.Run("error event during abort is suppressed", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{Status: agent.StatusBusy}
		m.aborting = true
		m.abortEntryIdx = 0
		m.entries = []displayEntry{{kind: entryStatus, content: "Cancelling..."}}

		m.handleEvent(agent.Event{
			Type:      agent.EventError,
			SessionID: m.sessionID,
			Data:      agent.ErrorData{Message: "MessageAbortedError"},
		})

		// Should NOT have appended an error entry.
		if len(m.entries) != 1 {
			t.Errorf("expected 1 entry (error suppressed), got %d", len(m.entries))
		}
	})

	t.Run("error event when not aborting is shown", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{Status: agent.StatusBusy}
		m.aborting = false

		m.handleEvent(agent.Event{
			Type:      agent.EventError,
			SessionID: m.sessionID,
			Data:      agent.ErrorData{Message: "some real error"},
		})

		found := false
		for _, e := range m.entries {
			if e.kind == entryError && e.content == "some real error" {
				found = true
			}
		}
		if !found {
			t.Error("expected error entry to be appended when not aborting")
		}
	})

	t.Run("abort HTTP failure updates entry to Cancel failed", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.aborting = true
		m.abortEntryIdx = 0
		m.entries = []displayEntry{{kind: entryStatus, content: "Cancelling..."}}

		m.Update(sessionAbortResultMsg{err: fmt.Errorf("connection refused")})

		if m.entries[0].content != "Cancel failed" {
			t.Errorf("entry = %q, want %q", m.entries[0].content, "Cancel failed")
		}
		if m.aborting {
			t.Error("expected aborting=false after failure")
		}
		if m.err == nil {
			t.Error("expected m.err to be set")
		}
	})
}

// TestBuildHelpText_ShowsCancelWhenBusy verifies the help bar includes
// the cancel hint when the agent is busy.
func TestBuildHelpText_ShowsCancelWhenBusy(t *testing.T) {
	t.Parallel()

	t.Run("busy shows cancel hint", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{Status: agent.StatusBusy}

		help := m.buildHelpText()
		if !strings.Contains(help, "ctrl+c: cancel") {
			t.Errorf("expected help to contain 'ctrl+c: cancel', got: %s", help)
		}
	})

	t.Run("starting shows cancel hint", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{Status: agent.StatusStarting}

		help := m.buildHelpText()
		if !strings.Contains(help, "ctrl+c: cancel") {
			t.Errorf("expected help to contain 'ctrl+c: cancel', got: %s", help)
		}
	})

	t.Run("idle does not show cancel hint", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{Status: agent.StatusIdle}

		help := m.buildHelpText()
		if strings.Contains(help, "ctrl+c: cancel") {
			t.Errorf("expected help to NOT contain 'ctrl+c: cancel' when idle, got: %s", help)
		}
	})

	t.Run("nil info does not show cancel hint", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = nil

		help := m.buildHelpText()
		if strings.Contains(help, "ctrl+c: cancel") {
			t.Errorf("expected help to NOT contain 'ctrl+c: cancel' with nil info, got: %s", help)
		}
	})

	t.Run("double-tap hint overrides normal help", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{Status: agent.StatusIdle}
		m.lastCtrlC = time.Now()

		help := m.buildHelpText()
		if !strings.Contains(help, "press ctrl+c again to quit") {
			t.Errorf("expected double-tap hint, got: %s", help)
		}
	})
}

// TestSpinnerTickSurvivesSessionConfirmDialog is a regression test for the bug
// where opening a confirm dialog in SessionViewModel swallowed spinner.TickMsg,
// permanently breaking the spinner's self-sustaining tick chain.
func TestSpinnerTickSurvivesSessionConfirmDialog(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "test-session")
	m.showConfirm = true
	m.confirm = newConfirmDialog("Mark done?", "Are you sure?", "done")

	// Generate a valid tick message from the spinner's own state.
	tickMsg := m.spinner.Tick()

	_, cmd := m.Update(tickMsg)

	// The spinner must schedule the next tick (non-nil cmd) to keep
	// the animation alive.
	if cmd == nil {
		t.Fatal("spinner tick was swallowed by confirm dialog; expected a follow-up tick command")
	}

	// The returned command should produce another spinner.TickMsg.
	nextMsg := cmd()
	if _, ok := nextMsg.(spinner.TickMsg); !ok {
		t.Fatalf("expected spinner.TickMsg, got %T", nextMsg)
	}
}

// TestSession_WordBackwardOnEmptyInput is a regression test for an upstream
// bug in bubbles textarea.wordLeft() that causes an infinite loop when the
// cursor is at position (0,0) — i.e. when the input is empty. Without the
// workaround this test hangs forever.
func TestSession_WordBackwardOnEmptyInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{name: "alt+b", msg: tea.KeyPressMsg{Code: 'b', Mod: tea.ModAlt}},
		{name: "alt+left", msg: tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := NewSessionViewModel(nil, "test-session")
			m.width = 80
			m.height = 40
			m.inputActive = true
			m.input.Focus()

			// Must return immediately instead of hanging.
			model, _ := m.Update(tt.msg)
			m = model.(*SessionViewModel)
			if m.input.Value() != "" {
				t.Fatalf("expected empty input, got %q", m.input.Value())
			}
		})
	}
}

// --- diffLines tests ---

func TestDiffLines_IdenticalInput(t *testing.T) {
	t.Parallel()
	lines := []string{"a", "b", "c"}
	ops := diffLines(lines, lines)
	for i, op := range ops {
		if op.op != diffEqual {
			t.Fatalf("op[%d]: expected diffEqual, got %d", i, op.op)
		}
		if op.text != lines[i] {
			t.Fatalf("op[%d]: expected %q, got %q", i, lines[i], op.text)
		}
	}
}

func TestDiffLines_PureDeletion(t *testing.T) {
	t.Parallel()
	old := []string{"a", "b", "c"}
	ops := diffLines(old, nil)
	if len(ops) != 3 {
		t.Fatalf("expected 3 ops, got %d", len(ops))
	}
	for _, op := range ops {
		if op.op != diffDelete {
			t.Fatalf("expected diffDelete, got %d", op.op)
		}
	}
}

func TestDiffLines_PureInsertion(t *testing.T) {
	t.Parallel()
	newLines := []string{"x", "y"}
	ops := diffLines(nil, newLines)
	if len(ops) != 2 {
		t.Fatalf("expected 2 ops, got %d", len(ops))
	}
	for _, op := range ops {
		if op.op != diffInsert {
			t.Fatalf("expected diffInsert, got %d", op.op)
		}
	}
}

func TestDiffLines_SingleLineChange(t *testing.T) {
	t.Parallel()
	old := []string{"hello world"}
	newLines := []string{"hello there"}
	ops := diffLines(old, newLines)
	// No common lines, so expect delete then insert.
	if len(ops) != 2 {
		t.Fatalf("expected 2 ops, got %d: %+v", len(ops), ops)
	}
	if ops[0].op != diffDelete || ops[0].text != "hello world" {
		t.Fatalf("expected delete 'hello world', got %+v", ops[0])
	}
	if ops[1].op != diffInsert || ops[1].text != "hello there" {
		t.Fatalf("expected insert 'hello there', got %+v", ops[1])
	}
}

func TestDiffLines_WithContext(t *testing.T) {
	t.Parallel()
	old := []string{"ctx before", "old line", "ctx after"}
	newLines := []string{"ctx before", "new line", "ctx after"}
	ops := diffLines(old, newLines)

	// Should be: equal, delete, insert, equal.
	expected := []struct {
		op   diffOp
		text string
	}{
		{diffEqual, "ctx before"},
		{diffDelete, "old line"},
		{diffInsert, "new line"},
		{diffEqual, "ctx after"},
	}
	if len(ops) != len(expected) {
		t.Fatalf("expected %d ops, got %d: %+v", len(expected), len(ops), ops)
	}
	for i, e := range expected {
		if ops[i].op != e.op || ops[i].text != e.text {
			t.Fatalf("op[%d]: expected {%d, %q}, got {%d, %q}", i, e.op, e.text, ops[i].op, ops[i].text)
		}
	}
}

func TestDiffLines_MultiLineChange(t *testing.T) {
	t.Parallel()
	old := []string{
		"\tswitch p.Tool {",
		"\tcase \"Read\", \"Write\", \"Edit\":",
		"\t\tif fp, ok := p.Input[\"filePath\"].(string); ok {",
		"\t\t\treturn fp",
		"\t\t}",
		"\tcase \"Glob\":",
	}
	newLines := []string{
		"\tswitch strings.ToLower(p.Tool) {",
		"\tcase \"read\", \"write\", \"edit\":",
		"\t\tif fp, ok := p.Input[\"filePath\"].(string); ok {",
		"\t\t\treturn fp",
		"\t\t}",
		"\tcase \"glob\":",
	}
	ops := diffLines(old, newLines)

	// Lines 3-5 are identical context. Lines 1,2,6 are changed.
	var deletes, inserts, equals int
	for _, op := range ops {
		switch op.op {
		case diffEqual:
			equals++
		case diffDelete:
			deletes++
		case diffInsert:
			inserts++
		}
	}
	if equals != 3 {
		t.Errorf("expected 3 equal ops, got %d", equals)
	}
	if deletes != 3 {
		t.Errorf("expected 3 deletes, got %d", deletes)
	}
	if inserts != 3 {
		t.Errorf("expected 3 inserts, got %d", inserts)
	}
}

// --- highlightLinePair tests ---

func TestHighlightLinePair_CommonPrefixSuffix(t *testing.T) {
	t.Parallel()
	// Use plain styles (no ANSI) so we can inspect text structure.
	noop := lipgloss.NewStyle()
	oldR, newR := highlightLinePair(
		"case \"Read\":",
		"case \"read\":",
		"    ", noop, noop, noop,
	)
	// Both should contain the diff marker.
	if !strings.Contains(oldR, "- ") {
		t.Errorf("old line missing '- ' prefix: %q", oldR)
	}
	if !strings.Contains(newR, "+ ") {
		t.Errorf("new line missing '+ ' prefix: %q", newR)
	}
}

func TestHighlightLinePair_FullyDifferent(t *testing.T) {
	t.Parallel()
	noop := lipgloss.NewStyle()
	oldR, newR := highlightLinePair("aaa", "bbb", "", noop, noop, noop)
	if !strings.Contains(oldR, "- ") {
		t.Errorf("old line missing '- ': %q", oldR)
	}
	if !strings.Contains(newR, "+ ") {
		t.Errorf("new line missing '+ ': %q", newR)
	}
}

func TestHighlightLinePair_IdenticalLines(t *testing.T) {
	t.Parallel()
	noop := lipgloss.NewStyle()
	oldR, newR := highlightLinePair("same", "same", "  ", noop, noop, noop)
	// Common prefix covers entire string; diff middle is empty.
	if !strings.Contains(oldR, "- same") {
		t.Errorf("expected old to contain '- same': %q", oldR)
	}
	if !strings.Contains(newR, "+ same") {
		t.Errorf("expected new to contain '+ same': %q", newR)
	}
}

// --- toolSummary / relPath tests ---

func TestToolSummary_ReadWithLineRange(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "test")
	m.projectDir = "/Users/me/project"

	p := agent.Part{
		Tool: "Read",
		Input: map[string]any{
			"filePath": "/Users/me/project/src/main.go",
			"offset":   float64(100),
			"limit":    float64(20),
		},
	}
	got := m.toolSummary(p)
	if got != "src/main.go:100-119" {
		t.Errorf("expected 'src/main.go:100-119', got %q", got)
	}
}

func TestToolSummary_ReadOffsetOnly(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "test")
	m.projectDir = "/Users/me/project"

	p := agent.Part{
		Tool: "read", // lowercase (OpenCode)
		Input: map[string]any{
			"filePath": "/Users/me/project/src/main.go",
			"offset":   float64(50),
		},
	}
	got := m.toolSummary(p)
	if got != "src/main.go:50" {
		t.Errorf("expected 'src/main.go:50', got %q", got)
	}
}

func TestToolSummary_ReadNoOffsetNoLimit(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "test")
	m.projectDir = "/Users/me/project"

	p := agent.Part{
		Tool:  "Read",
		Input: map[string]any{"filePath": "/Users/me/project/src/main.go"},
	}
	got := m.toolSummary(p)
	if got != "src/main.go" {
		t.Errorf("expected 'src/main.go', got %q", got)
	}
}

func TestToolSummary_EditRelativePath(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "test")
	m.projectDir = "/Users/me/project"

	p := agent.Part{
		Tool:  "edit",
		Input: map[string]any{"filePath": "/Users/me/project/internal/foo.go"},
	}
	got := m.toolSummary(p)
	if got != "internal/foo.go" {
		t.Errorf("expected 'internal/foo.go', got %q", got)
	}
}

func TestToolSummary_PathOutsideProject(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "test")
	m.projectDir = "/Users/me/project"

	p := agent.Part{
		Tool:  "Write",
		Input: map[string]any{"filePath": "/tmp/other/file.go"},
	}
	got := m.toolSummary(p)
	// Path outside project dir should be returned as-is.
	if got != "/tmp/other/file.go" {
		t.Errorf("expected '/tmp/other/file.go', got %q", got)
	}
}

func TestRelPath_NoProjectDir(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "test")
	// projectDir is empty.
	got := m.relPath("/some/absolute/path.go")
	if got != "/some/absolute/path.go" {
		t.Errorf("expected unchanged path, got %q", got)
	}
}

func TestRelPath_EmptyInput(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "test")
	m.projectDir = "/Users/me/project"
	got := m.relPath("")
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// --- renderTodoList tests ---

func TestRenderTodoList_RendersAllStatuses(t *testing.T) {
	t.Parallel()
	dim := lipgloss.NewStyle()
	p := agent.Part{
		Tool: "TodoWrite",
		Input: map[string]any{
			"todos": []interface{}{
				map[string]interface{}{"content": "Done task", "status": "completed", "priority": "high"},
				map[string]interface{}{"content": "Working on it", "status": "in_progress", "priority": "high"},
				map[string]interface{}{"content": "Not started", "status": "pending", "priority": "medium"},
				map[string]interface{}{"content": "Dropped", "status": "cancelled", "priority": "low"},
			},
		},
	}
	lines := renderTodoList(p, "  ", dim)
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}
	// Check that each line contains the correct icon and content.
	expects := []struct {
		icon    string
		content string
	}{
		{"✓", "Done task"},
		{"◉", "Working on it"},
		{"○", "Not started"},
		{"✗", "Dropped"},
	}
	for i, e := range expects {
		if !strings.Contains(lines[i], e.icon) {
			t.Errorf("line %d: expected icon %q, got %q", i, e.icon, lines[i])
		}
		if !strings.Contains(lines[i], e.content) {
			t.Errorf("line %d: expected content %q, got %q", i, e.content, lines[i])
		}
	}
}

func TestRenderTodoList_EmptyTodos(t *testing.T) {
	t.Parallel()
	dim := lipgloss.NewStyle()
	p := agent.Part{
		Tool:  "todowrite",
		Input: map[string]any{"todos": []interface{}{}},
	}
	lines := renderTodoList(p, "  ", dim)
	if lines != nil {
		t.Errorf("expected nil for empty todos, got %v", lines)
	}
}

func TestColonOpensActionMenu_OnUserMessage(t *testing.T) {
	t.Parallel()
	colon := tea.KeyPressMsg{Code: ':'}

	t.Run("opens menu on user entry with messageID", func(t *testing.T) {
		t.Parallel()
		entries := []displayEntry{
			{kind: entryUser, content: "hello world", messageID: "msg-123"},
			{kind: entryText, content: "agent response"},
		}
		m := newTestSessionModel(entries)
		m.cursor = 0

		_, _ = m.handleKey(colon)

		if !m.showMenu {
			t.Fatal("expected showMenu=true after ':' on user message")
		}
		if m.menuMessageID != "msg-123" {
			t.Errorf("menuMessageID = %q, want %q", m.menuMessageID, "msg-123")
		}
		if m.menuMessageContent != "hello world" {
			t.Errorf("menuMessageContent = %q, want %q", m.menuMessageContent, "hello world")
		}
	})

	t.Run("does not open menu on user entry without messageID", func(t *testing.T) {
		t.Parallel()
		// User entries added inline (follow-up) don't have messageIDs
		// until history is reloaded. Colon should be a no-op.
		entries := []displayEntry{
			{kind: entryUser, content: "follow-up"},
		}
		m := newTestSessionModel(entries)
		m.cursor = 0

		_, _ = m.handleKey(colon)

		if m.showMenu {
			t.Error("expected showMenu=false for user entry without messageID")
		}
	})

	t.Run("does not open menu on entry without messageID", func(t *testing.T) {
		t.Parallel()
		entries := []displayEntry{
			{kind: entryUser, content: "user msg", messageID: "msg-1"},
			{kind: entryText, content: "agent response"},
		}
		m := newTestSessionModel(entries)
		m.cursor = 1 // on the agent text entry (no messageID)

		_, _ = m.handleKey(colon)

		if m.showMenu {
			t.Error("expected showMenu=false for entry without messageID")
		}
	})

	t.Run("opens menu on agent entry with messageID", func(t *testing.T) {
		t.Parallel()
		entries := []displayEntry{
			{kind: entryUser, content: "prompt", messageID: "msg-1"},
			{kind: entryText, content: "agent response", messageID: "msg-2"},
		}
		m := newTestSessionModel(entries)
		m.cursor = 1 // on the agent text entry (has messageID)

		_, _ = m.handleKey(colon)

		if !m.showMenu {
			t.Fatal("expected showMenu=true after ':' on agent entry with messageID")
		}
		if m.menuMessageID != "msg-2" {
			t.Errorf("menuMessageID = %q, want %q", m.menuMessageID, "msg-2")
		}
	})

	t.Run("enter does not open menu (no regression)", func(t *testing.T) {
		t.Parallel()
		entries := []displayEntry{
			{kind: entryUser, content: "prompt", messageID: "msg-42"},
		}
		m := newTestSessionModel(entries)
		m.cursor = 0

		enter := tea.KeyPressMsg{Code: tea.KeyEnter}
		_, _ = m.handleKey(enter)

		if m.showMenu {
			t.Error("expected showMenu=false — Enter should not open action menu")
		}
	})
}

func TestActionMenu_RevertTriggersConfirmDialog(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel([]displayEntry{
		{kind: entryUser, content: "original prompt", messageID: "msg-10"},
	})
	m.cursor = 0
	m.menuMessageID = "msg-10"
	m.menuMessageContent = "original prompt"
	m.showMenu = true

	// Simulate selecting "revert" from the action menu.
	cmd := m.handleMenuAction("revert")

	if cmd != nil {
		t.Error("expected no command from handleMenuAction (it opens a confirm dialog)")
	}
	if !m.showConfirm {
		t.Fatal("expected showConfirm=true after selecting revert")
	}
}

func TestHandleSessionMessages_PopulatesMessageID(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	messages := []agent.MessageData{
		{
			ID:   "msg-abc",
			Role: "user",
			Parts: []agent.Part{
				{Type: agent.PartText, Text: "hello"},
			},
		},
		{
			ID:   "msg-def",
			Role: "assistant",
			Parts: []agent.Part{
				{ID: "p1", Type: agent.PartText, Text: "world"},
			},
		},
		{
			ID:   "msg-ghi",
			Role: "user",
			Parts: []agent.Part{
				{Type: agent.PartText, Text: "follow-up"},
			},
		},
	}

	m.handleSessionMessages(messages)

	// Should have 3 entries: user, text, user
	if len(m.entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(m.entries))
	}

	// First user entry should have messageID.
	if m.entries[0].messageID != "msg-abc" {
		t.Errorf("entries[0].messageID = %q, want %q", m.entries[0].messageID, "msg-abc")
	}
	if m.entries[0].kind != entryUser {
		t.Errorf("entries[0].kind = %d, want entryUser", m.entries[0].kind)
	}

	// Assistant text entry should have messageID.
	if m.entries[1].messageID != "msg-def" {
		t.Errorf("entries[1].messageID = %q, want %q", m.entries[1].messageID, "msg-def")
	}
	if m.entries[1].kind != entryText {
		t.Errorf("entries[1].kind = %d, want entryText", m.entries[1].kind)
	}

	// Second user entry should have messageID.
	if m.entries[2].messageID != "msg-ghi" {
		t.Errorf("entries[2].messageID = %q, want %q", m.entries[2].messageID, "msg-ghi")
	}
}

func TestHandleSessionMessages_PopulatesNextMessageID(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	messages := []agent.MessageData{
		{
			ID:   "msg-1",
			Role: "user",
			Parts: []agent.Part{
				{Type: agent.PartText, Text: "first"},
			},
		},
		{
			ID:   "msg-2",
			Role: "assistant",
			Parts: []agent.Part{
				{ID: "p1", Type: agent.PartText, Text: "reply"},
			},
		},
		{
			ID:   "msg-3",
			Role: "user",
			Parts: []agent.Part{
				{Type: agent.PartText, Text: "second"},
			},
		},
		{
			ID:   "msg-4",
			Role: "assistant",
			Parts: []agent.Part{
				{ID: "p2", Type: agent.PartText, Text: "final reply"},
			},
		},
	}

	m.handleSessionMessages(messages)

	// Should have 4 entries: user, text, user, text
	if len(m.entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(m.entries))
	}

	// msg-1 → next is msg-2
	if m.entries[0].nextMessageID != "msg-2" {
		t.Errorf("entries[0].nextMessageID = %q, want %q", m.entries[0].nextMessageID, "msg-2")
	}
	// msg-2 → next is msg-3
	if m.entries[1].nextMessageID != "msg-3" {
		t.Errorf("entries[1].nextMessageID = %q, want %q", m.entries[1].nextMessageID, "msg-3")
	}
	// msg-3 → next is msg-4
	if m.entries[2].nextMessageID != "msg-4" {
		t.Errorf("entries[2].nextMessageID = %q, want %q", m.entries[2].nextMessageID, "msg-4")
	}
	// msg-4 is last → nextMessageID should be empty (fork entire session)
	if m.entries[3].nextMessageID != "" {
		t.Errorf("entries[3].nextMessageID = %q, want empty (last message)", m.entries[3].nextMessageID)
	}
}

func TestBuildHelpText_ShowsActionsHint(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.info = &agent.SessionInfo{Status: agent.StatusIdle}

	help := m.buildHelpText()
	if !strings.Contains(help, ":: actions") {
		t.Errorf("expected help to contain ':: actions', got: %s", help)
	}
}

func TestHandleMessage_BackfillsUserMessageID(t *testing.T) {
	t.Parallel()

	t.Run("backfills messageID on most recent user entry without one", func(t *testing.T) {
		t.Parallel()
		// Simulate: user sent a message (inline entry, no messageID),
		// then SSE delivers the user message event with the server-assigned ID.
		entries := []displayEntry{
			{kind: entryUser, content: "first", messageID: "msg-1"},
			{kind: entryText, content: "response"},
			{kind: entryUser, content: "second"}, // no messageID yet
		}
		m := newTestSessionModel(entries)

		m.handleMessage(agent.MessageData{
			ID:   "msg-2",
			Role: "user",
		})

		if m.entries[2].messageID != "msg-2" {
			t.Errorf("entries[2].messageID = %q, want %q", m.entries[2].messageID, "msg-2")
		}
		// Ensure earlier entry was not modified.
		if m.entries[0].messageID != "msg-1" {
			t.Errorf("entries[0].messageID = %q, want %q (should be unchanged)", m.entries[0].messageID, "msg-1")
		}
	})

	t.Run("no-op when all user entries already have messageID", func(t *testing.T) {
		t.Parallel()
		entries := []displayEntry{
			{kind: entryUser, content: "first", messageID: "msg-1"},
		}
		m := newTestSessionModel(entries)

		m.handleMessage(agent.MessageData{
			ID:   "msg-dup",
			Role: "user",
		})

		// Should not overwrite existing messageID.
		if m.entries[0].messageID != "msg-1" {
			t.Errorf("entries[0].messageID = %q, want %q", m.entries[0].messageID, "msg-1")
		}
	})

	t.Run("no-op when SSE user message has no ID", func(t *testing.T) {
		t.Parallel()
		entries := []displayEntry{
			{kind: entryUser, content: "pending"},
		}
		m := newTestSessionModel(entries)

		m.handleMessage(agent.MessageData{
			Role: "user",
		})

		if m.entries[0].messageID != "" {
			t.Errorf("entries[0].messageID = %q, want empty", m.entries[0].messageID)
		}
	})
}

func TestHandleSessionMessages_ReplacesEntries(t *testing.T) {
	t.Parallel()

	// Simulate a session with 3 messages, then revert to the first user
	// message. After handleSessionMessages with the post-revert data,
	// entries should reflect only the first user message.
	entries := []displayEntry{
		{kind: entryUser, content: "first prompt", messageID: "msg-1"},
		{kind: entryText, content: "agent response"},
		{kind: entryUser, content: "follow-up", messageID: "msg-2"},
		{kind: entryText, content: "second response"},
	}
	m := newTestSessionModel(entries)
	m.historyLoaded = true

	if len(m.entries) != 4 {
		t.Fatalf("pre-condition: expected 4 entries, got %d", len(m.entries))
	}

	// Simulate post-revert message list: only the first user message remains.
	postRevertMessages := []agent.MessageData{
		{
			ID:   "msg-1",
			Role: "user",
			Parts: []agent.Part{
				{Type: agent.PartText, Text: "first prompt"},
			},
		},
	}

	m.handleSessionMessages(postRevertMessages)

	if len(m.entries) != 1 {
		t.Fatalf("expected 1 entry after revert, got %d", len(m.entries))
	}
	if m.entries[0].kind != entryUser {
		t.Errorf("entries[0].kind = %d, want entryUser", m.entries[0].kind)
	}
	if m.entries[0].messageID != "msg-1" {
		t.Errorf("entries[0].messageID = %q, want %q", m.entries[0].messageID, "msg-1")
	}
	if m.entries[0].content != "first prompt" {
		t.Errorf("entries[0].content = %q, want %q", m.entries[0].content, "first prompt")
	}
}

// allMessages returns a full message history as the OpenCode server would
// return it — always all messages, regardless of revert state.
func allMessages() []agent.MessageData {
	return []agent.MessageData{
		{
			ID:   "msg-1",
			Role: "user",
			Parts: []agent.Part{
				{Type: agent.PartText, Text: "first prompt"},
			},
		},
		{
			ID:   "msg-2",
			Role: "assistant",
			Parts: []agent.Part{
				{ID: "p1", Type: agent.PartText, Text: "first response"},
			},
		},
		{
			ID:   "msg-3",
			Role: "user",
			Parts: []agent.Part{
				{Type: agent.PartText, Text: "follow-up"},
			},
		},
		{
			ID:   "msg-4",
			Role: "assistant",
			Parts: []agent.Part{
				{ID: "p2", Type: agent.PartText, Text: "second response"},
			},
		},
	}
}

func TestHandleSessionMessages_FiltersByRevertMessageID(t *testing.T) {
	t.Parallel()

	t.Run("no revert shows all messages", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{} // RevertMessageID is empty

		m.handleSessionMessages(allMessages())

		// 4 messages: user, text, user, text
		if len(m.entries) != 4 {
			t.Fatalf("expected 4 entries, got %d", len(m.entries))
		}
	})

	t.Run("revert to msg-3 hides msg-3 and msg-4", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{RevertMessageID: "msg-3"}

		m.handleSessionMessages(allMessages())

		// Should only show msg-1 (user) and msg-2 (assistant text)
		if len(m.entries) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(m.entries))
		}
		if m.entries[0].messageID != "msg-1" {
			t.Errorf("entries[0].messageID = %q, want %q", m.entries[0].messageID, "msg-1")
		}
		if m.entries[1].kind != entryText {
			t.Errorf("entries[1].kind = %d, want entryText", m.entries[1].kind)
		}
	})

	t.Run("revert to msg-1 hides everything", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{RevertMessageID: "msg-1"}

		m.handleSessionMessages(allMessages())

		if len(m.entries) != 0 {
			t.Fatalf("expected 0 entries when reverting to first message, got %d", len(m.entries))
		}
	})

	t.Run("revert to nonexistent ID shows all messages", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{RevertMessageID: "msg-nonexistent"}

		m.handleSessionMessages(allMessages())

		// No matching ID means no filtering — all messages shown.
		if len(m.entries) != 4 {
			t.Fatalf("expected 4 entries, got %d", len(m.entries))
		}
	})

	t.Run("nil info shows all messages", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = nil

		m.handleSessionMessages(allMessages())

		if len(m.entries) != 4 {
			t.Fatalf("expected 4 entries, got %d", len(m.entries))
		}
	})
}

func TestEventRevertChange_UpdatesInfoRevertMessageID(t *testing.T) {
	t.Parallel()

	t.Run("sets RevertMessageID from SSE event", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{}

		m.handleEvent(agent.Event{
			Type:      agent.EventRevertChange,
			SessionID: m.sessionID,
			Data:      agent.RevertChangeData{MessageID: "msg-42"},
		})

		if m.info.RevertMessageID != "msg-42" {
			t.Errorf("RevertMessageID = %q, want %q", m.info.RevertMessageID, "msg-42")
		}
	})

	t.Run("clears RevertMessageID on unrevert", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = &agent.SessionInfo{RevertMessageID: "msg-old"}

		m.handleEvent(agent.Event{
			Type:      agent.EventRevertChange,
			SessionID: m.sessionID,
			Data:      agent.RevertChangeData{MessageID: ""},
		})

		if m.info.RevertMessageID != "" {
			t.Errorf("RevertMessageID = %q, want empty", m.info.RevertMessageID)
		}
	})

	t.Run("no-op when info is nil", func(t *testing.T) {
		t.Parallel()
		m := newTestSessionModel(nil)
		m.info = nil

		// Should not panic.
		m.handleEvent(agent.Event{
			Type:      agent.EventRevertChange,
			SessionID: m.sessionID,
			Data:      agent.RevertChangeData{MessageID: "msg-42"},
		})
	})
}

func TestSendMessage_ClearsRevertMessageID(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.info = &agent.SessionInfo{RevertMessageID: "msg-99"}
	m.inputActive = true
	m.input.Focus()
	m.input.SetValue("new message")

	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	_, _ = m.handleKey(enter)

	if m.info.RevertMessageID != "" {
		t.Errorf("RevertMessageID = %q, want empty after sending message", m.info.RevertMessageID)
	}
}

func TestActionMenu_ForkFromUserMessage(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel([]displayEntry{
		{kind: entryUser, content: "original prompt", messageID: "msg-10"},
	})
	m.cursor = 0
	m.menuMessageID = "msg-10"
	m.menuMessageContent = "original prompt"
	m.showMenu = true

	// Simulate selecting "fork" from the action menu.
	cmd := m.handleMenuAction("fork")

	// Fork is non-destructive — no confirm dialog, returns a command directly.
	if m.showConfirm {
		t.Error("fork should not show a confirm dialog")
	}
	if cmd == nil {
		t.Fatal("expected a command from handleMenuAction(fork)")
	}
}

func TestActionMenu_ForkFromAssistantMessage(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel([]displayEntry{
		{kind: entryUser, content: "prompt", messageID: "msg-1"},
		{kind: entryText, content: "agent response", messageID: "msg-2"},
	})
	m.cursor = 1
	m.menuMessageID = "msg-2"
	m.showMenu = true

	cmd := m.handleMenuAction("fork")

	if m.showConfirm {
		t.Error("fork should not show a confirm dialog")
	}
	if cmd == nil {
		t.Fatal("expected a command from handleMenuAction(fork)")
	}
}

func TestActionMenu_UserMessageHasRevertAndFork(t *testing.T) {
	t.Parallel()
	colon := tea.KeyPressMsg{Code: ':'}

	entries := []displayEntry{
		{kind: entryUser, content: "hello", messageID: "msg-1"},
	}
	m := newTestSessionModel(entries)
	m.cursor = 0

	_, _ = m.handleKey(colon)

	if !m.showMenu {
		t.Fatal("expected menu to open")
	}
	// User messages should have both revert and fork.
	items := m.menu.items
	if len(items) != 2 {
		t.Fatalf("expected 2 menu items, got %d", len(items))
	}
	if items[0].action != "revert" {
		t.Errorf("first item action = %q, want revert", items[0].action)
	}
	if items[1].action != "fork" {
		t.Errorf("second item action = %q, want fork", items[1].action)
	}
}

func TestActionMenu_AssistantMessageHasForkOnly(t *testing.T) {
	t.Parallel()
	colon := tea.KeyPressMsg{Code: ':'}

	entries := []displayEntry{
		{kind: entryText, content: "agent response", messageID: "msg-2"},
	}
	m := newTestSessionModel(entries)
	m.cursor = 0

	_, _ = m.handleKey(colon)

	if !m.showMenu {
		t.Fatal("expected menu to open")
	}
	// Assistant messages should only have fork (no revert).
	items := m.menu.items
	if len(items) != 1 {
		t.Fatalf("expected 1 menu item, got %d", len(items))
	}
	if items[0].action != "fork" {
		t.Errorf("item action = %q, want fork", items[0].action)
	}
}

func TestForkResultMsg_ErrorSetsErr(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	errMsg := forkResultMsg{err: fmt.Errorf("fork failed")}

	result, _ := m.Update(errMsg)
	updated := result.(*SessionViewModel)

	if updated.err == nil {
		t.Fatal("expected error to be set")
	}
	if updated.err.Error() != "fork failed" {
		t.Errorf("err = %q, want %q", updated.err.Error(), "fork failed")
	}
}

func TestForkResultMsg_SuccessEmitsOpenForkedSessionMsg(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.width = 80
	m.height = 40
	successMsg := forkResultMsg{sessionID: "new-session-id"}

	_, cmd := m.Update(successMsg)

	if cmd == nil {
		t.Fatal("expected a command to navigate to the forked session")
	}
	// Execute the command and verify it produces openForkedSessionMsg.
	msg := cmd()
	forkNav, ok := msg.(openForkedSessionMsg)
	if !ok {
		t.Fatalf("expected openForkedSessionMsg, got %T", msg)
	}
	if forkNav.sessionID != "new-session-id" {
		t.Errorf("sessionID = %q, want %q", forkNav.sessionID, "new-session-id")
	}
}

func TestRenderEntry_PermResultGranted(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel([]displayEntry{
		{kind: entryPermResult, content: "Allowed bash: echo hello", permGranted: true},
	})
	m.width = 80

	lines := m.buildContentLines()

	// entryPermResult is navigable; when selected it gets a border.
	// Should have at least 1 content line.
	if len(lines) == 0 {
		t.Error("expected at least 1 line for permission result")
	}

	// Verify the rendered output contains the checkmark.
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "✓") {
		t.Error("expected ✓ in granted permission result")
	}
}

func TestRenderEntry_PermResultDenied(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel([]displayEntry{
		{kind: entryPermResult, content: "Denied bash: echo hello", permGranted: false},
	})
	m.width = 80

	lines := m.buildContentLines()

	if len(lines) == 0 {
		t.Error("expected at least 1 line for permission result")
	}

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "✗") {
		t.Error("expected ✗ in denied permission result")
	}
}

func TestRenderEntry_ErrorMultilineWraps(t *testing.T) {
	t.Parallel()

	longErr := strings.Repeat("x", 200)
	m := newTestSessionModel([]displayEntry{
		{kind: entryError, content: longErr},
	})
	m.width = 80

	lines := m.buildContentLines()

	// With width=80, a 200-char error must wrap.
	if len(lines) < 3 {
		t.Errorf("expected at least 3 lines for wrapped error, got %d", len(lines))
	}
	if m.entryEndLine[0] != len(lines) {
		t.Errorf("entryEndLine[0] = %d, want %d", m.entryEndLine[0], len(lines))
	}
}

func TestPendingPermissionMsg_RestoresPrompt(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	perms := []agent.PermissionData{
		{RequestID: "perm-42", Tool: "bash", Description: "echo hello"},
	}

	result, _ := m.Update(pendingPermissionMsg{perms: perms})
	updated := result.(*SessionViewModel)

	if len(updated.pendingPerms) != 1 {
		t.Fatalf("expected 1 pending perm, got %d", len(updated.pendingPerms))
	}
	if updated.pendingPerms[0].RequestID != "perm-42" {
		t.Errorf("RequestID = %q, want %q", updated.pendingPerms[0].RequestID, "perm-42")
	}

	// Should NOT have added any entry — the active prompt is rendered
	// virtually at the bottom of buildContentLines instead.
	for _, e := range updated.entries {
		if e.kind == entryPermResult {
			t.Error("did not expect an entryPermResult entry from pendingPermissionMsg (only from user response)")
		}
	}
}

func TestPendingPermissionMsg_ReplacesQueue(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	// Pre-set a pending permission (arrived via live SSE).
	m.pendingPerms = []agent.PermissionData{
		{RequestID: "existing", Tool: "read_file"},
		{RequestID: "stale", Tool: "write_file"},
	}
	initialEntryCount := len(m.entries)

	// Daemon says only one perm remains (the other was auto-rejected).
	result, _ := m.Update(pendingPermissionMsg{perms: []agent.PermissionData{
		{RequestID: "existing", Tool: "read_file"},
	}})
	updated := result.(*SessionViewModel)

	// Queue is replaced with the daemon's authoritative state.
	if len(updated.pendingPerms) != 1 {
		t.Fatalf("expected 1 pending perm, got %d", len(updated.pendingPerms))
	}
	if updated.pendingPerms[0].RequestID != "existing" {
		t.Errorf("perms[0].RequestID = %q, want %q", updated.pendingPerms[0].RequestID, "existing")
	}
	// No new entries should have been added.
	if len(updated.entries) != initialEntryCount {
		t.Errorf("expected %d entries (unchanged), got %d", initialEntryCount, len(updated.entries))
	}
}

func TestPendingPermissionMsg_EmptyIsNoop(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)

	result, _ := m.Update(pendingPermissionMsg{perms: nil})
	updated := result.(*SessionViewModel)

	if len(updated.pendingPerms) != 0 {
		t.Errorf("expected 0 pending perms, got %d", len(updated.pendingPerms))
	}
}

func TestBuildContentLines_VirtualPermPrompt(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel([]displayEntry{
		{kind: entryText, content: "I will read a file."},
	})
	m.width = 80
	m.pendingPerms = []agent.PermissionData{
		{RequestID: "p1", Tool: "Read", Description: "~/.config/app.toml"},
		{RequestID: "p2", Tool: "Write", Description: "~/.config/app.toml"},
	}

	lines := m.buildContentLines()
	joined := strings.Join(lines, "\n")

	// The virtual prompt for the first pending perm should appear.
	if !strings.Contains(joined, "Read") {
		t.Error("expected virtual prompt to contain tool name 'Read'")
	}
	if !strings.Contains(joined, "~/.config/app.toml") {
		t.Error("expected virtual prompt to contain description")
	}
	// Queue counter should show (1/2).
	if !strings.Contains(joined, "(1/2)") {
		t.Error("expected queue counter (1/2) in virtual prompt")
	}
	// Should NOT show the second permission prompt.
	if strings.Contains(joined, "Write") {
		t.Error("expected only the first pending perm to be shown, but found 'Write'")
	}
	// Should contain the [y]/[n] hint.
	if !strings.Contains(joined, "[y]") || !strings.Contains(joined, "[n]") {
		t.Error("expected [y] Allow / [n] Deny hint in virtual prompt")
	}
}

func TestPermission_NavigationLockedWhilePending(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel([]displayEntry{
		{kind: entryText, content: "first message"},
		{kind: entryText, content: "second message"},
	})
	m.width = 80
	m.cursor = 1 // start at second entry
	m.pendingPerms = []agent.PermissionData{
		{RequestID: "p1", Tool: "Read", Description: "some file"},
	}

	// Try pressing 'k' (up) — cursor should NOT move.
	m.handleKey(tea.KeyPressMsg{Code: 'k'})
	if m.cursor != 1 {
		t.Errorf("cursor moved to %d during pending permission, expected 1", m.cursor)
	}

	// Try pressing 'j' (down) — cursor should NOT move.
	m.handleKey(tea.KeyPressMsg{Code: 'j'})
	if m.cursor != 1 {
		t.Errorf("cursor moved to %d during pending permission, expected 1", m.cursor)
	}

	// Try pressing 'm' (message) — input should NOT activate.
	m.handleKey(tea.KeyPressMsg{Code: 'm'})
	if m.inputActive {
		t.Error("input activated during pending permission")
	}
}

func TestPermission_ReplyInFlightBlocksDoubleRespond(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.width = 80
	m.pendingPerms = []agent.PermissionData{
		{RequestID: "p1", Tool: "Read", Description: "file1"},
		{RequestID: "p2", Tool: "Write", Description: "file2"},
	}

	// Press 'y' — should set replyingPermID and fire a command.
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: 'y'})
	if m.replyingPermID != "p1" {
		t.Fatalf("replyingPermID = %q, want %q", m.replyingPermID, "p1")
	}
	if cmd == nil {
		t.Fatal("expected a command from permission reply")
	}
	// Queue should NOT have been popped yet.
	if len(m.pendingPerms) != 2 {
		t.Errorf("pendingPerms len = %d, want 2 (not yet popped)", len(m.pendingPerms))
	}

	// Press 'y' again while reply is in flight — should be no-op.
	_, cmd2 := m.handleKey(tea.KeyPressMsg{Code: 'y'})
	if cmd2 != nil {
		t.Error("expected no command while reply is in flight")
	}
}

func TestPermission_ReplyResultPopsQueue(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.width = 80
	m.pendingPerms = []agent.PermissionData{
		{RequestID: "p1", Tool: "Read", Description: "file1"},
		{RequestID: "p2", Tool: "Write", Description: "file2"},
	}
	m.replyingPermID = "p1"

	// Simulate successful grant reply.
	result, _ := m.Update(permissionReplyResultMsg{
		perm:  agent.PermissionData{RequestID: "p1", Tool: "Read", Description: "file1"},
		allow: true,
	})
	updated := result.(*SessionViewModel)

	if updated.replyingPermID != "" {
		t.Errorf("replyingPermID = %q, want empty", updated.replyingPermID)
	}
	if len(updated.pendingPerms) != 1 {
		t.Fatalf("expected 1 remaining perm, got %d", len(updated.pendingPerms))
	}
	if updated.pendingPerms[0].RequestID != "p2" {
		t.Errorf("remaining perm = %q, want %q", updated.pendingPerms[0].RequestID, "p2")
	}
	// Should have appended a granted result entry.
	found := false
	for _, e := range updated.entries {
		if e.kind == entryPermResult && e.permGranted {
			found = true
		}
	}
	if !found {
		t.Error("expected a granted entryPermResult entry")
	}
}

func TestPermission_DenyMarksRunningToolsFailed(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel([]displayEntry{
		{
			kind: entryTool,
			toolPart: &agent.Part{
				ID:     "tool-1",
				Type:   agent.PartToolCall,
				Tool:   "glob",
				Status: agent.PartRunning,
				Input:  map[string]any{"path": "/Users/test"},
			},
		},
		{
			kind: entryTool,
			toolPart: &agent.Part{
				ID:     "tool-2",
				Type:   agent.PartToolCall,
				Tool:   "read",
				Status: agent.PartPending,
				Input:  map[string]any{"filePath": "/Users/test/file"},
			},
		},
	})
	m.width = 80
	m.pendingPerms = []agent.PermissionData{{RequestID: "p1", Tool: "external_directory", Description: "/Users/test/*"}}
	m.replyingPermID = "p1"

	result, cmd := m.Update(permissionReplyResultMsg{
		perm:  agent.PermissionData{RequestID: "p1", Tool: "external_directory", Description: "/Users/test/*"},
		allow: false,
	})
	updated := result.(*SessionViewModel)

	if cmd == nil {
		t.Fatal("expected follow-up fetch commands on denial")
	}
	for i, e := range updated.entries {
		if e.kind != entryTool || e.toolPart == nil {
			continue
		}
		if e.toolPart.Status != agent.PartFailed {
			t.Fatalf("tool entry %d status = %q, want %q", i, e.toolPart.Status, agent.PartFailed)
		}
	}
}

func TestRenderEntry_NilToolPartNoPanic(t *testing.T) {
	t.Parallel()

	// An entryTool with nil toolPart should not panic.
	m := newTestSessionModel([]displayEntry{
		{kind: entryText, content: "some text"},
		{kind: entryTool, content: "[tool] something", toolPart: nil},
	})
	m.width = 80

	// This previously panicked with nil pointer dereference.
	lines := m.buildContentLines()
	if len(lines) == 0 {
		t.Error("expected at least 1 line")
	}
}

func TestBuildContentLines_VirtualPermPromptHasBorder(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.width = 80
	m.pendingPerms = []agent.PermissionData{
		{RequestID: "p1", Tool: "Read", Description: "~/.config/app.toml"},
	}

	lines := m.buildContentLines()
	joined := strings.Join(lines, "\n")

	// Should contain the rounded border characters.
	if !strings.Contains(joined, "╭") || !strings.Contains(joined, "╰") {
		t.Error("expected orange rounded border (╭/╰) around permission prompt")
	}
	if !strings.Contains(joined, "Permission") {
		t.Error("expected 'Permission' header in prompt")
	}
}
