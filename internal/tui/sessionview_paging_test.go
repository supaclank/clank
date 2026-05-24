package tui

import (
	"strings"
	"testing"
)

// TestPageUpDown_ExactPagingViaEntryStartLine verifies that pgup/pgdn
// jump the cursor by a real viewport's worth of rendered content
// (using entryStartLine) rather than the legacy contentHeight()/4
// heuristic which over- or under-paged depending on entry density.
func TestPageUpDown_ExactPagingViaEntryStartLine(t *testing.T) {
	t.Parallel()

	// Build many navigable entries with predictable height (1 line each
	// after trimming) so we know exactly how far one page should jump.
	const entryCount = 200
	entries := make([]displayEntry, entryCount)
	for i := range entries {
		entries[i] = displayEntry{kind: entryUser, content: "msg " + strings.Repeat("x", 5)}
	}
	m := newTestSessionModel(entries)
	m.buildContentLines()

	page := m.contentHeight()
	if page < 5 {
		t.Fatalf("contentHeight too small for test: %d", page)
	}

	t.Run("pgdown jumps at least one viewport down", func(t *testing.T) {
		m.cursor = 0
		startLine := m.entryStartLine[m.cursor]
		idx := m.pageDownNavigableEntry(m.cursor)
		if idx < 0 || idx >= len(m.entries) {
			t.Fatalf("pageDown returned invalid idx=%d", idx)
		}
		jumped := m.entryStartLine[idx] - startLine
		if jumped < page {
			t.Errorf("pgdown jumped only %d lines; want >= page (%d)", jumped, page)
		}
	})

	t.Run("pgup jumps at least one viewport up", func(t *testing.T) {
		m.cursor = entryCount - 1
		startLine := m.entryStartLine[m.cursor]
		idx := m.pageUpNavigableEntry(m.cursor)
		if idx < 0 || idx >= len(m.entries) {
			t.Fatalf("pageUp returned invalid idx=%d", idx)
		}
		jumped := startLine - m.entryStartLine[idx]
		if jumped < page {
			t.Errorf("pgup jumped only %d lines; want >= page (%d)", jumped, page)
		}
	})

	t.Run("pgup near top clamps to firstNavigableEntry", func(t *testing.T) {
		m.cursor = 1
		idx := m.pageUpNavigableEntry(m.cursor)
		if idx != m.firstNavigableEntry() {
			t.Errorf("pgup near top: idx=%d, want firstNavigableEntry=%d", idx, m.firstNavigableEntry())
		}
	})

	t.Run("pgdown near bottom clamps to lastNavigableEntry", func(t *testing.T) {
		m.cursor = entryCount - 2
		idx := m.pageDownNavigableEntry(m.cursor)
		if idx != m.lastNavigableEntry() {
			t.Errorf("pgdown near bottom: idx=%d, want lastNavigableEntry=%d", idx, m.lastNavigableEntry())
		}
	})
}

// TestPageUpDown_SkipsNonNavigable verifies that paging only lands on
// navigable entries even when the gap is full of status/tool rows.
func TestPageUpDown_SkipsNonNavigable(t *testing.T) {
	t.Parallel()

	// Sandwich navigable entries between large blocks of non-navigable
	// rows so the natural jump target is inside a skip-block.
	var entries []displayEntry
	entries = append(entries, displayEntry{kind: entryUser, content: "first"})
	for i := 0; i < 100; i++ {
		entries = append(entries, displayEntry{kind: entryStatus, content: "status"})
	}
	entries = append(entries, displayEntry{kind: entryUser, content: "middle"})
	for i := 0; i < 100; i++ {
		entries = append(entries, displayEntry{kind: entryTool, content: "[tool] x"})
	}
	entries = append(entries, displayEntry{kind: entryUser, content: "last"})

	m := newTestSessionModel(entries)
	m.buildContentLines()

	m.cursor = 0
	idx := m.pageDownNavigableEntry(m.cursor)
	if !isNavigable(m.entries[idx].kind) {
		t.Errorf("pgdown landed on non-navigable kind=%v at idx=%d", m.entries[idx].kind, idx)
	}

	m.cursor = len(entries) - 1
	idx = m.pageUpNavigableEntry(m.cursor)
	if !isNavigable(m.entries[idx].kind) {
		t.Errorf("pgup landed on non-navigable kind=%v at idx=%d", m.entries[idx].kind, idx)
	}
}
