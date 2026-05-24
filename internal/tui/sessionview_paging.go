package tui

// pageUpNavigableEntry returns the index of the navigable entry whose
// rendered start line is at least one viewport above the cursor's start
// line. Falls back to firstNavigableEntry() when nothing qualifies (i.e.
// the cursor is already within one page of the top).
//
// Uses entryStartLine populated by the most recent buildContentLines,
// so paging is exact relative to actually-rendered line counts rather
// than the contentHeight()/4 heuristic that pre-dated this helper.
func (m *SessionViewModel) pageUpNavigableEntry(from int) int {
	page := m.contentHeight()
	if page < 1 || from <= 0 || from >= len(m.entryStartLine) {
		return m.prevNavigableEntry(from)
	}
	target := m.entryStartLine[from] - page
	if target < 0 {
		target = 0
	}
	for i := from - 1; i >= 0; i-- {
		if !isNavigable(m.entries[i].kind) {
			continue
		}
		if i >= len(m.entryStartLine) {
			continue
		}
		if m.entryStartLine[i] <= target {
			return i
		}
	}
	return m.firstNavigableEntry()
}

// pageDownNavigableEntry returns the index of the navigable entry whose
// rendered start line is at least one viewport below the cursor's start
// line. Falls back to lastNavigableEntry() when nothing qualifies.
func (m *SessionViewModel) pageDownNavigableEntry(from int) int {
	page := m.contentHeight()
	if page < 1 || from < 0 || from >= len(m.entryStartLine) {
		return m.nextNavigableEntry(from)
	}
	target := m.entryStartLine[from] + page
	for i := from + 1; i < len(m.entries); i++ {
		if !isNavigable(m.entries[i].kind) {
			continue
		}
		if i >= len(m.entryStartLine) {
			continue
		}
		if m.entryStartLine[i] >= target {
			return i
		}
	}
	return m.lastNavigableEntry()
}
