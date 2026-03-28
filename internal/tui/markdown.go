package tui

import (
	"strings"

	"github.com/charmbracelet/glamour"
	styles "github.com/charmbracelet/glamour/styles"
)

// newMarkdownRenderer creates a glamour TermRenderer configured for the given
// content width. The renderer uses the dark style with TrueColor support.
// Callers should create a new renderer when the terminal width changes.
func newMarkdownRenderer(width int) *glamour.TermRenderer {
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(styles.DarkStyle),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		// glamour.NewTermRenderer only fails on invalid style paths;
		// DarkStyle is a builtin constant so this should never happen.
		return nil
	}
	return r
}

// renderMarkdown converts a markdown string into ANSI-styled terminal output
// at the given width. If rendering fails, the raw text is returned via
// wrapText as a fallback.
func renderMarkdown(content string, width int) string {
	r := newMarkdownRenderer(width)
	if r == nil {
		return wrapText(content, width)
	}
	out, err := r.Render(content)
	if err != nil {
		return wrapText(content, width)
	}
	// glamour appends trailing newlines; trim them so the caller controls spacing.
	return strings.TrimRight(out, "\n")
}
