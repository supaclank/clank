package tui

import (
	"testing"
)

func TestTextSelection_StartAndClear(t *testing.T) {
	t.Parallel()

	var s textSelection
	s.Start(5, 10)

	if !s.active {
		t.Fatal("expected active after Start")
	}
	if s.startX != 5 || s.startY != 10 {
		t.Fatalf("start coords: got (%d,%d), want (5,10)", s.startX, s.startY)
	}
	// Not a real selection yet — same start/end.
	if s.HasSelection() {
		t.Fatal("expected no selection when start == end")
	}

	s.Update(15, 12)
	if !s.HasSelection() {
		t.Fatal("expected selection after Update moves endpoint")
	}

	s.Clear()
	if s.active || s.HasSelection() {
		t.Fatal("expected inactive after Clear")
	}
}

func TestTextSelection_Normalized_TopToBottom(t *testing.T) {
	t.Parallel()

	var s textSelection
	// Drag downward: (5,2) → (10,5)
	s.Start(5, 2)
	s.Update(10, 5)
	sx, sy, ex, ey := s.normalized()
	if sx != 5 || sy != 2 || ex != 10 || ey != 5 {
		t.Fatalf("normalized downward drag: got (%d,%d)-(%d,%d), want (5,2)-(10,5)", sx, sy, ex, ey)
	}
}

func TestTextSelection_Normalized_BottomToTop(t *testing.T) {
	t.Parallel()

	var s textSelection
	// Drag upward: (10,5) → (5,2) — should normalize to (5,2)→(10,5)
	s.Start(10, 5)
	s.Update(5, 2)
	sx, sy, ex, ey := s.normalized()
	if sx != 5 || sy != 2 || ex != 10 || ey != 5 {
		t.Fatalf("normalized upward drag: got (%d,%d)-(%d,%d), want (5,2)-(10,5)", sx, sy, ex, ey)
	}
}

func TestTextSelection_Normalized_SameLineRightToLeft(t *testing.T) {
	t.Parallel()

	var s textSelection
	s.Start(20, 3)
	s.Update(5, 3)
	sx, sy, ex, ey := s.normalized()
	if sx != 5 || sy != 3 || ex != 20 || ey != 3 {
		t.Fatalf("normalized same-line rtl: got (%d,%d)-(%d,%d), want (5,3)-(20,3)", sx, sy, ex, ey)
	}
}

func TestTextSelection_ContainsCell_SingleLine(t *testing.T) {
	t.Parallel()

	var s textSelection
	s.Start(5, 3)
	s.Update(10, 3)

	tests := []struct {
		x, y int
		want bool
	}{
		{4, 3, false},  // before selection
		{5, 3, true},   // start col inclusive
		{7, 3, true},   // middle
		{9, 3, true},   // last col (end is exclusive at 10)
		{10, 3, false}, // end col exclusive
		{7, 2, false},  // row above
		{7, 4, false},  // row below
	}

	for _, tt := range tests {
		got := s.ContainsCell(tt.x, tt.y)
		if got != tt.want {
			t.Errorf("ContainsCell(%d,%d) = %v, want %v", tt.x, tt.y, got, tt.want)
		}
	}
}

func TestTextSelection_ContainsCell_MultiLine(t *testing.T) {
	t.Parallel()

	var s textSelection
	s.Start(5, 2)
	s.Update(10, 4)

	tests := []struct {
		x, y int
		want bool
	}{
		{4, 2, false},  // before start col on start row
		{5, 2, true},   // start col on start row
		{50, 2, true},  // past end on start row — selected to EOL
		{0, 3, true},   // beginning of middle row
		{50, 3, true},  // end of middle row — fully selected
		{0, 4, true},   // beginning of end row
		{9, 4, true},   // within end col
		{10, 4, false}, // at end col — exclusive
		{0, 1, false},  // row before selection
		{0, 5, false},  // row after selection
	}

	for _, tt := range tests {
		got := s.ContainsCell(tt.x, tt.y)
		if got != tt.want {
			t.Errorf("ContainsCell(%d,%d) = %v, want %v", tt.x, tt.y, got, tt.want)
		}
	}
}

func TestSubstringByColumn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		start, end int
		want       string
	}{
		{"full", "hello world", 0, 11, "hello world"},
		{"middle", "hello world", 2, 7, "llo w"},
		{"start only", "hello", 0, 3, "hel"},
		{"end only", "hello", 3, 5, "lo"},
		{"empty range", "hello", 3, 3, ""},
		{"past end", "hi", 0, 10, "hi"},
		{"negative start", "hello", -1, 3, "hel"},
		{"start past length", "hi", 5, 10, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := substringByColumn(tt.input, tt.start, tt.end)
			if got != tt.want {
				t.Errorf("substringByColumn(%q, %d, %d) = %q, want %q",
					tt.input, tt.start, tt.end, got, tt.want)
			}
		})
	}
}

func TestTrimBorderChars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no borders", "hello world", "hello world"},
		{"left border", "│ hello", "hello"},
		{"right border", "hello │", "hello"},
		{"both borders", "│ hello │", "hello"},
		{"rounded top", "╭──────╮", ""},
		{"rounded bottom", "╰──────╯", ""},
		{"border with padding", "│  some text  │", "some text"},
		{"normal border", "┌──────┐", ""},
		{"indented text", "  Agent:", "Agent:"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := trimBorderChars(tt.input)
			if got != tt.want {
				t.Errorf("trimBorderChars(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTextSelection_Extract_SingleLine(t *testing.T) {
	t.Parallel()

	contentLines := []string{
		"  first line of content",
		"  second line here",
		"  third line too",
	}

	var s textSelection
	// Select part of the second line (screen row 3 = headerRows(2) + line 1).
	// headerRows=2, scrollOffset=0, so screen row 2 → content line 0.
	s.Start(4, 3)
	s.Update(12, 3)

	got := s.Extract(contentLines, 2, 0)
	// Content line 1 = "  second line here", cols 4..12 = "cond lin"
	// After trimBorderChars: "cond lin" (no border chars to trim)
	want := "cond lin"
	if got != want {
		t.Errorf("Extract single line: got %q, want %q", got, want)
	}
}

func TestTextSelection_Extract_MultiLine(t *testing.T) {
	t.Parallel()

	contentLines := []string{
		"  Hello world",
		"  This is a test",
		"  Final line",
	}

	var s textSelection
	// Select from middle of first line to middle of last line.
	// headerRows=2, scrollOffset=0
	// Screen row 2 → content line 0, screen row 4 → content line 2.
	s.Start(8, 2)
	s.Update(10, 4)

	got := s.Extract(contentLines, 2, 0)
	// Line 0 cols 8..end: "world" → trimBorderChars → "world"
	// Line 1 cols 0..end: "  This is a test" → trimBorderChars → "This is a test"
	// Line 2 cols 0..10: "  Final li" → trimBorderChars → "Final li"
	want := "world\nThis is a test\nFinal li"
	if got != want {
		t.Errorf("Extract multi line:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestTextSelection_Extract_WithBorderLines(t *testing.T) {
	t.Parallel()

	contentLines := []string{
		"",
		"╭──────────────╮",
		"│ Agent:       │",
		"│ Hello world  │",
		"╰──────────────╯",
		"  Some other text",
	}

	var s textSelection
	// Select the entire bordered block (screen rows 2..6, headerRows=2, scrollOffset=0).
	s.Start(0, 2)
	s.Update(20, 7)

	got := s.Extract(contentLines, 2, 0)
	// Line 0: "" → trimBorderChars → ""
	// Line 1: "╭──────────────╮" → all border → ""
	// Line 2: "│ Agent:       │" → "Agent:"
	// Line 3: "│ Hello world  │" → "Hello world"
	// Line 4: "╰──────────────╯" → ""
	// Line 5: "  Some other text" → "Some other text"
	// Joined with \n, but empty lines get included.
	want := "\n\nAgent:\nHello world\n\nSome other text"
	if got != want {
		t.Errorf("Extract with borders:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestTextSelection_Extract_WithScrollOffset(t *testing.T) {
	t.Parallel()

	contentLines := []string{
		"  line zero",
		"  line one",
		"  line two",
		"  line three",
		"  line four",
	}

	var s textSelection
	// scrollOffset=2 means content lines 2,3,4 are visible starting at headerRows.
	// Screen row 2 (headerRows=2) → content line 2 (scrollOffset + 0).
	s.Start(0, 2)
	s.Update(20, 3)

	got := s.Extract(contentLines, 2, 2)
	// Line 2: "  line two" → "line two"
	// Line 3: "  line three" cols 0..20 → "line three"
	want := "line two\nline three"
	if got != want {
		t.Errorf("Extract with scroll offset:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestTextSelection_Extract_OutOfBounds(t *testing.T) {
	t.Parallel()

	contentLines := []string{"  hello"}

	var s textSelection
	// Selection entirely above content area.
	s.Start(0, 0)
	s.Update(5, 0)
	got := s.Extract(contentLines, 2, 0)
	if got != "" {
		t.Errorf("Extract above content: got %q, want empty", got)
	}
}

func TestTextSelection_Finish_ClearsSelection(t *testing.T) {
	t.Parallel()

	contentLines := []string{"  hello world"}

	var s textSelection
	s.Start(2, 2)
	s.Update(7, 2)

	text := s.Finish(contentLines, 2, 0)
	if text == "" {
		t.Error("Finish should return extracted text")
	}
	if s.active {
		t.Error("Finish should clear the selection")
	}
}

func TestHighlightLine_NoSelection(t *testing.T) {
	t.Parallel()

	var s textSelection
	line := "hello world"
	got := s.highlightLine(line, 5)
	if got != line {
		t.Errorf("highlightLine with no selection: got %q, want %q", got, line)
	}
}

func TestHighlightLine_SelectedRow(t *testing.T) {
	t.Parallel()

	var s textSelection
	s.Start(2, 5)
	s.Update(7, 5)

	line := "hello world"
	got := s.highlightLine(line, 5)

	// The highlighted line should contain the highlight-on and highlight-off sequences.
	highlightOn := "\x1b[48;2;59;66;97m"
	highlightOff := "\x1b[49m"
	if len(got) <= len(line) {
		t.Fatal("highlighted line should be longer than original due to ANSI codes")
	}
	if !contains(got, highlightOn) {
		t.Error("highlighted line should contain highlight-on ANSI sequence")
	}
	if !contains(got, highlightOff) {
		t.Error("highlighted line should contain highlight-off ANSI sequence")
	}
}

func TestHighlightLine_UnselectedRow(t *testing.T) {
	t.Parallel()

	var s textSelection
	s.Start(2, 5)
	s.Update(7, 5)

	line := "hello world"
	got := s.highlightLine(line, 10) // row 10 is outside selection (row 5)
	if got != line {
		t.Errorf("highlightLine on unselected row: got %q, want %q", got, line)
	}
}

// TestHighlightLine_SurvivesSGRReset is a regression test for the bug where
// Lipgloss's \x1b[m (SGR reset) after styled spans would clear the selection
// background, causing the highlight to appear only on the border character but
// not on the text content inside.
func TestHighlightLine_SurvivesSGRReset(t *testing.T) {
	t.Parallel()

	// Simulate a Lipgloss-rendered bordered line:
	// \x1b[38;2;124;58;237m│\x1b[m Hello world \x1b[38;2;124;58;237m│\x1b[m
	line := "\x1b[38;2;124;58;237m│\x1b[m Hello world \x1b[38;2;124;58;237m│\x1b[m"

	var s textSelection
	// Multi-line selection: this is a middle row, so full line should be highlighted.
	s.Start(0, 3)
	s.Update(20, 7)

	got := s.highlightLine(line, 5)
	highlightOn := "\x1b[48;2;59;66;97m"

	// Count how many times highlightOn appears — it must be re-emitted after
	// each SGR reset to keep the background alive across styled spans.
	count := 0
	for idx := 0; idx <= len(got)-len(highlightOn); idx++ {
		if got[idx:idx+len(highlightOn)] == highlightOn {
			count++
		}
	}

	// There are 3 ANSI sequences in the input (\x1b[38;...m, \x1b[m, \x1b[38;...m, \x1b[m).
	// The highlight must be re-applied after each one, plus the initial emission.
	// So we expect at least 3 occurrences (initial + 2 re-applications after the two \x1b[m resets).
	if count < 3 {
		t.Errorf("highlightOn should appear at least 3 times (initial + re-applied after SGR resets), got %d\nfull output: %q", count, got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
