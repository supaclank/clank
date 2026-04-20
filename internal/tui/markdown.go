package tui

import (
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	styles "github.com/charmbracelet/glamour/styles"
)

// markdownRendererPools stores one sync.Pool per content width. A TermRenderer
// is expensive to construct (parses style JSON, builds chroma state) but holds
// an internal buffer so it is NOT safe for concurrent use; a pool lets us
// reuse renderers sequentially without paying the construction cost each call.
// Width rarely changes so the outer map stays tiny.
var markdownRendererPools sync.Map // map[int]*sync.Pool

func getMarkdownRendererPool(width int) *sync.Pool {
	if v, ok := markdownRendererPools.Load(width); ok {
		return v.(*sync.Pool)
	}
	p := &sync.Pool{
		New: func() any {
			r, err := glamour.NewTermRenderer(
				glamour.WithStandardStyle(styles.DarkStyle),
				glamour.WithWordWrap(width),
			)
			if err != nil {
				// DarkStyle is a builtin; this branch should be unreachable.
				return (*glamour.TermRenderer)(nil)
			}
			return r
		},
	}
	actual, _ := markdownRendererPools.LoadOrStore(width, p)
	return actual.(*sync.Pool)
}

// renderMarkdown converts a markdown string into ANSI-styled terminal output
// at the given width. If rendering fails, the raw text is returned via
// wrapText as a fallback.
func renderMarkdown(content string, width int) string {
	pool := getMarkdownRendererPool(width)
	r, _ := pool.Get().(*glamour.TermRenderer)
	if r == nil {
		return wrapText(content, width)
	}
	defer pool.Put(r)
	out, err := r.Render(content)
	if err != nil {
		return wrapText(content, width)
	}
	// glamour appends trailing newlines; trim them so the caller controls spacing.
	return strings.TrimRight(out, "\n")
}
