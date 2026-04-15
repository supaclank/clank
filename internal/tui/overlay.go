package tui

import "charm.land/lipgloss/v2"

// overlayCenter renders popup centered on top of base using lipgloss's
// cell-aware Compositor. Unlike the previous byte-splicing approach this
// correctly handles ANSI escape sequences and preserves background content
// on both sides of the popup.
func overlayCenter(base, popup string, width, height int) string {
	popupW := lipgloss.Width(popup)
	popupH := lipgloss.Height(popup)

	x := (width - popupW) / 2
	if x < 0 {
		x = 0
	}
	y := (height - popupH) / 2
	if y < 0 {
		y = 0
	}

	// Size the background layer to the full terminal area so the
	// compositor canvas covers the entire screen.
	bg := lipgloss.NewLayer(
		lipgloss.NewStyle().MaxWidth(width).Height(height).Render(base),
	).Z(0)
	fg := lipgloss.NewLayer(popup).X(x).Y(y).Z(1)

	return lipgloss.NewCompositor(bg, fg).Render()
}
