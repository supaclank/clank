package tui

import (
	tea "charm.land/bubbletea/v2"
)

// HandleWheel moves the sidebar cursor by one row in the direction of
// the wheel button, clamped to the list bounds. Returns whether the
// cursor actually moved.
//
// Wheel scroll moves cursor by 1 row per tick (matching modelpicker's
// list semantics), independent of the chat's wheelScrollLines step
// size — sidebar rows are taller and a per-tick row move keeps the
// gesture predictable. No wraparound: wheel scroll past the ends
// stops, while keyboard up/down (handleKey) wraps because that's an
// explicit gesture.
func (m *SidebarModel) HandleWheel(button tea.MouseButton) bool {
	maxIdx := len(m.flat) - 1
	if maxIdx < 0 {
		return false
	}
	switch button {
	case tea.MouseWheelUp:
		if m.cursor > 0 {
			m.cursor--
			return true
		}
	case tea.MouseWheelDown:
		if m.cursor < maxIdx {
			m.cursor++
			return true
		}
	}
	return false
}
