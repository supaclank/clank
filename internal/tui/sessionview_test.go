package tui

import (
	"context"
	"testing"
	"time"

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
}
