package tui

import (
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
)

// textSelection tracks mouse-drag text selection state within the session view.
// Screen coordinates are mapped to content lines via headerRows and scrollOffset,
// and the plain text is extracted by stripping ANSI codes from the rendered lines.
type textSelection struct {
	active bool // drag in progress
	startX int  // screen col where drag began
	startY int  // screen row where drag began
	endX   int  // screen col of current/end position
	endY   int  // screen row of current/end position
}

// Start begins a new selection at the given screen coordinates.
func (s *textSelection) Start(x, y int) {
	s.active = true
	s.startX = x
	s.startY = y
	s.endX = x
	s.endY = y
}

// Update moves the selection endpoint during a drag.
func (s *textSelection) Update(x, y int) {
	if !s.active {
		return
	}
	s.endX = x
	s.endY = y
}

// Clear resets the selection.
func (s *textSelection) Clear() {
	*s = textSelection{}
}

// HasSelection returns true if there is a non-zero selection in progress.
func (s *textSelection) HasSelection() bool {
	return s.active && (s.startX != s.endX || s.startY != s.endY)
}

// normalized returns the selection coordinates ordered top-to-bottom, left-to-right.
func (s *textSelection) normalized() (startX, startY, endX, endY int) {
	sy, ey := s.startY, s.endY
	sx, ex := s.startX, s.endX
	if sy > ey || (sy == ey && sx > ex) {
		sy, ey = ey, sy
		sx, ex = ex, sx
	}
	return sx, sy, ex, ey
}

// Finish completes the selection, extracts the selected text from contentLines,
// copies it to the system clipboard, and clears the selection.
// headerRows is the number of screen rows before the content area.
// scrollOffset is the first visible content line index.
func (s *textSelection) Finish(contentLines []string, headerRows, scrollOffset int) string {
	if !s.HasSelection() {
		s.Clear()
		return ""
	}

	text := s.Extract(contentLines, headerRows, scrollOffset)
	s.Clear()

	if text == "" {
		return ""
	}

	// Best-effort clipboard write; ignore errors (e.g. no clipboard available).
	_ = clipboard.WriteAll(text)
	return text
}

// Extract returns the plain text for the current selection region.
// It maps screen coordinates to content line indices and strips ANSI codes,
// then extracts the selected column range from each line.
func (s *textSelection) Extract(contentLines []string, headerRows, scrollOffset int) string {
	if !s.HasSelection() {
		return ""
	}

	sx, sy, ex, ey := s.normalized()

	// Convert screen rows to content line indices.
	firstLine := sy - headerRows + scrollOffset
	lastLine := ey - headerRows + scrollOffset

	if firstLine < 0 {
		firstLine = 0
		sx = 0
	}
	if lastLine < 0 {
		return ""
	}
	if firstLine >= len(contentLines) {
		return ""
	}
	if lastLine >= len(contentLines) {
		lastLine = len(contentLines) - 1
	}

	var result strings.Builder

	for lineIdx := firstLine; lineIdx <= lastLine; lineIdx++ {
		plain := ansi.Strip(contentLines[lineIdx])

		// Determine column range for this line.
		colStart := 0
		colEnd := utf8.RuneCountInString(plain)

		if lineIdx == firstLine {
			colStart = sx
		}
		if lineIdx == lastLine {
			colEnd = ex
		}

		extracted := substringByColumn(plain, colStart, colEnd)
		extracted = trimBorderChars(extracted)

		if lineIdx > firstLine {
			result.WriteString("\n")
		}
		result.WriteString(extracted)
	}

	return result.String()
}

// ContainsCell returns true if the given screen cell is within the selection.
func (s *textSelection) ContainsCell(screenX, screenY int) bool {
	if !s.HasSelection() {
		return false
	}

	sx, sy, ex, ey := s.normalized()

	if screenY < sy || screenY > ey {
		return false
	}
	if sy == ey {
		// Single line: check column range.
		return screenX >= sx && screenX < ex
	}
	if screenY == sy {
		return screenX >= sx
	}
	if screenY == ey {
		return screenX < ex
	}
	// Middle lines are fully selected.
	return true
}

// substringByColumn extracts a substring from s between column positions
// start (inclusive) and end (exclusive), counting by rune.
func substringByColumn(s string, start, end int) string {
	if start < 0 {
		start = 0
	}

	runes := []rune(s)
	if start >= len(runes) {
		return ""
	}
	if end > len(runes) {
		end = len(runes)
	}
	if start >= end {
		return ""
	}
	return string(runes[start:end])
}

// borderRunes is the set of box-drawing characters used by Lipgloss borders
// that we strip from the edges of selected text.
var borderRunes = map[rune]bool{
	'│': true, '╭': true, '╮': true, '╰': true, '╯': true, '─': true,
	'┌': true, '┐': true, '└': true, '┘': true,
}

// trimBorderChars removes leading and trailing border/box-drawing characters
// and any adjacent padding spaces from a line of selected text.
func trimBorderChars(s string) string {
	runes := []rune(s)

	// Trim leading border chars and spaces.
	start := 0
	for start < len(runes) && (borderRunes[runes[start]] || runes[start] == ' ') {
		start++
	}

	// Trim trailing border chars and spaces.
	end := len(runes)
	for end > start && (borderRunes[runes[end-1]] || runes[end-1] == ' ') {
		end--
	}

	// If we trimmed everything, the line was pure border — return empty.
	if start >= end {
		return ""
	}

	return string(runes[start:end])
}

// selectionHighlightStyle is the background color applied to selected text.
var selectionHighlightStyle = lipgloss.NewStyle().Background(lipgloss.Color("#3B4261"))

// highlightLine applies selection highlighting to a rendered content line.
// screenRow is the screen Y coordinate of this line. The function walks the
// rendered string character by character (skipping ANSI sequences) and wraps
// selected visible characters with the highlight background.
func (s *textSelection) highlightLine(rendered string, screenRow int) string {
	if !s.HasSelection() {
		return rendered
	}

	sx, sy, ex, ey := s.normalized()

	// Quick reject: line not in selection range at all.
	if screenRow < sy || screenRow > ey {
		return rendered
	}

	// Determine the selected column range for this screen row.
	colStart := 0
	colEnd := -1 // -1 means "to end of line"
	if sy == ey {
		// Single-line selection.
		colStart = sx
		colEnd = ex
	} else if screenRow == sy {
		colStart = sx
	} else if screenRow == ey {
		colEnd = ex
	}
	// Middle lines: colStart=0, colEnd=-1 (full line).

	// Walk the rendered string, tracking which visible column we're at.
	// ANSI escape sequences are invisible and don't advance the column counter.
	var result strings.Builder
	result.Grow(len(rendered) + 64) // a bit of room for added ANSI codes

	col := 0
	i := 0
	inSelection := false
	highlightOn := "\x1b[48;2;59;66;97m" // RGB background #3B4261
	highlightOff := "\x1b[49m"           // reset background

	for i < len(rendered) {
		// Check if we're at an ANSI escape sequence.
		if rendered[i] == '\x1b' {
			// Find the end of the escape sequence.
			j := i + 1
			if j < len(rendered) && rendered[j] == '[' {
				j++
				for j < len(rendered) && !((rendered[j] >= 'A' && rendered[j] <= 'Z') || (rendered[j] >= 'a' && rendered[j] <= 'z')) {
					j++
				}
				if j < len(rendered) {
					j++ // include the final letter
				}
			}
			// Write the escape sequence as-is, then re-apply the highlight
			// if we're inside the selection. Lipgloss emits \x1b[m (full SGR
			// reset) after styled spans, which clears our background color.
			result.WriteString(rendered[i:j])
			if inSelection {
				result.WriteString(highlightOn)
			}
			i = j
			continue
		}

		// Visible character — check if it's in the selection range.
		selected := col >= colStart && (colEnd < 0 || col < colEnd)

		if selected && !inSelection {
			result.WriteString(highlightOn)
			inSelection = true
		} else if !selected && inSelection {
			result.WriteString(highlightOff)
			inSelection = false
		}

		// Decode one rune and write it.
		r, size := utf8.DecodeRuneInString(rendered[i:])
		_ = r
		result.WriteString(rendered[i : i+size])
		col++
		i += size
	}

	if inSelection {
		result.WriteString(highlightOff)
	}

	return result.String()
}
