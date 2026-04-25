package tui

// Breakpoint navigation helpers shared across views (inbox, sidebar, ...).
//
// A "breakpoint" list is a sorted slice of cursor positions that
// shift+up/shift+down should snap between. The two helpers below are pure
// functions over the list and the current cursor, so they're easy to reuse
// from any view that defines its own notion of section boundaries.

// nextBreakpoint returns the smallest breakpoint strictly greater than cursor.
// If cursor is already at or past the last breakpoint, it returns the last one.
// Callers must pass a non-empty, ascending-sorted slice.
func nextBreakpoint(bp []int, cursor int) int {
	for _, b := range bp {
		if b > cursor {
			return b
		}
	}
	return bp[len(bp)-1]
}

// prevBreakpoint returns the largest breakpoint strictly less than cursor.
// If cursor is already at or before the first breakpoint, it returns the first one.
// Callers must pass a non-empty, ascending-sorted slice.
func prevBreakpoint(bp []int, cursor int) int {
	for i := len(bp) - 1; i >= 0; i-- {
		if bp[i] < cursor {
			return bp[i]
		}
	}
	return bp[0]
}
