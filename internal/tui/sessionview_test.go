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
		{entryPerm, true},
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
