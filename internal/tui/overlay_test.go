package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestOverlayCenter_Basic(t *testing.T) {
	t.Parallel()

	// 10x5 base filled with dots, 4x1 popup.
	base := strings.Repeat(strings.Repeat(".", 10)+"\n", 4) + strings.Repeat(".", 10)
	popup := "ABCD"

	result := overlayCenter(base, popup, 10, 5)
	lines := strings.Split(result, "\n")

	// Popup is 4 wide, 1 tall on a 10x5 canvas → row 2, col 3.
	if len(lines) < 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}

	mid := lines[2]
	plain := ansi.Strip(mid)
	if !strings.Contains(plain, "ABCD") {
		t.Errorf("expected popup text in middle row, got: %q", plain)
	}

	// Background dots should be preserved to the LEFT of the popup.
	if !strings.HasPrefix(plain, "...") {
		t.Errorf("expected background preserved left of popup, got: %q", plain)
	}

	// Background dots should be preserved to the RIGHT of the popup.
	if !strings.HasSuffix(plain, "...") {
		t.Errorf("expected background preserved right of popup, got: %q", plain)
	}
}

func TestOverlayCenter_PreservesANSI(t *testing.T) {
	t.Parallel()

	// Build a styled base line so the overlay must handle ANSI correctly.
	styled := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF0000")).Render("RED TEXT")
	// Pad to 20 wide, 3 tall.
	row := styled + strings.Repeat(" ", 20-lipgloss.Width(styled))
	base := row + "\n" + row + "\n" + row
	popup := "hi"

	result := overlayCenter(base, popup, 20, 3)
	lines := strings.Split(result, "\n")

	// The middle line (row 1) should contain both "hi" and "RED TEXT" fragments.
	plain := ansi.Strip(lines[1])
	if !strings.Contains(plain, "hi") {
		t.Errorf("expected popup in middle line, got: %q", plain)
	}

	// Non-overlaid rows should still contain the red text.
	plainTop := ansi.Strip(lines[0])
	if !strings.Contains(plainTop, "RED TEXT") {
		t.Errorf("expected styled base preserved on non-overlaid row, got: %q", plainTop)
	}
}

func TestOverlayCenter_LargePopup(t *testing.T) {
	t.Parallel()

	// Popup larger than canvas — should not panic, gets clamped to (0,0).
	base := "small"
	popup := strings.Repeat("X", 20)

	result := overlayCenter(base, popup, 10, 3)
	plain := ansi.Strip(result)
	// Should contain at least part of the popup without panicking.
	if !strings.Contains(plain, "X") {
		t.Errorf("expected popup content even when oversized, got: %q", plain)
	}
}

func TestOverlayCenter_EmptyPopup(t *testing.T) {
	t.Parallel()

	base := "hello\nworld"
	result := overlayCenter(base, "", 10, 2)
	plain := ansi.Strip(result)
	if !strings.Contains(plain, "hello") || !strings.Contains(plain, "world") {
		t.Errorf("empty popup should leave base intact, got: %q", plain)
	}
}

func TestOverlayCenter_MultiLinePopup(t *testing.T) {
	t.Parallel()

	base := strings.Repeat(strings.Repeat(".", 20)+"\n", 9) + strings.Repeat(".", 20)
	popup := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Render("OK")

	result := overlayCenter(base, popup, 20, 10)
	plain := ansi.Strip(result)

	// The bordered popup should appear somewhere in the output.
	if !strings.Contains(plain, "OK") {
		t.Errorf("expected bordered popup content, got:\n%s", plain)
	}

	// Background dots should still be present on rows outside the popup.
	lines := strings.Split(plain, "\n")
	topPlain := lines[0]
	if !strings.Contains(topPlain, "..........") {
		t.Errorf("expected background dots on top row, got: %q", topPlain)
	}
}

// TestOverlayCenter_OverlongLinesNotWrapped is a regression test for a bug
// where lines wider than the terminal (e.g. inbox rows with gap=1 timestamps)
// would word-wrap when the overlay applied Width(width), causing text like
// "2m ago" to spill onto the next line when a confirm dialog was toggled.
func TestOverlayCenter_OverlongLinesNotWrapped(t *testing.T) {
	t.Parallel()

	const width = 40
	const height = 5

	// Build a base where each line is wider than the canvas (50 > 40).
	// This simulates narrow-terminal inbox rows that trail past the edge.
	longLine := strings.Repeat("x", 50)
	base := strings.Join([]string{longLine, longLine, longLine, longLine, longLine}, "\n")

	popup := "OK"
	result := overlayCenter(base, popup, width, height)
	lines := strings.Split(result, "\n")

	// The output must still be exactly 5 lines — no wrapping.
	if len(lines) != height {
		t.Errorf("expected %d lines (no wrapping), got %d:\n%s", height, len(lines), result)
	}

	// Each non-popup line should be at most width visible chars (truncated).
	for i, line := range lines {
		vis := lipgloss.Width(line)
		if vis > width {
			t.Errorf("line %d visible width %d exceeds canvas width %d: %q", i, vis, width, ansi.Strip(line))
		}
	}
}
