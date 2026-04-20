package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// makeBenchModel constructs a SessionViewModel populated with n synthetic
// entries that mimic a long, realistic chat session: alternating user
// messages, assistant markdown text, tool calls, and the occasional
// thinking block. Width/height are sized like a typical full-screen terminal.
//
// The model is fully initialised: width/height are set, history is marked
// loaded, and the cursor is on the last entry. This is the state hot paths
// like buildContentLines and the wheel handler operate on in production.
func makeBenchModel(b *testing.B, n int) *SessionViewModel {
	b.Helper()
	m := NewSessionViewModel(nil, "bench-session")
	m.width = 120
	m.height = 40
	m.historyLoaded = true

	// Fixed assistant markdown body — non-trivial enough to exercise glamour
	// and wrapping, but stable so the per-entry cache can populate.
	const mdBody = `Here is **some** explanation with a list:

- first point with ` + "`code`" + `
- second point referencing ` + "`renderEntry`" + `
- third point about wrapping and longer text that should wrap across multiple lines on a 120-column terminal

` + "```go" + `
func example() {
    fmt.Println("hello, world")
}
` + "```" + `

And a closing sentence.`

	const userBody = "Please refactor the function and explain what you did. Make sure tests still pass."
	const thinkBody = "I should consider the existing structure before making changes."

	for i := 0; i < n; i++ {
		switch i % 5 {
		case 0:
			m.entries = append(m.entries, displayEntry{
				kind:      entryUser,
				partID:    fmt.Sprintf("u-%d", i),
				messageID: fmt.Sprintf("m-%d", i),
				content:   userBody,
				agent:     "default",
			})
		case 1:
			m.entries = append(m.entries, displayEntry{
				kind:      entryText,
				partID:    fmt.Sprintf("t-%d", i),
				messageID: fmt.Sprintf("m-%d", i),
				content:   mdBody,
			})
		case 2:
			m.entries = append(m.entries, displayEntry{
				kind:    entryTool,
				partID:  fmt.Sprintf("tool-%d", i),
				content: "[bash] echo hi",
				toolPart: &agent.Part{
					ID:     fmt.Sprintf("tool-%d", i),
					Type:   "tool",
					Tool:   "bash",
					Status: agent.PartCompleted,
					Input:  map[string]any{"command": "echo hi"},
					Output: "hi\n",
				},
			})
		case 3:
			m.entries = append(m.entries, displayEntry{
				kind:    entryThink,
				partID:  fmt.Sprintf("th-%d", i),
				content: thinkBody,
			})
		case 4:
			m.entries = append(m.entries, displayEntry{
				kind:    entryText,
				partID:  fmt.Sprintf("t2-%d", i),
				content: strings.Repeat("Short reply. ", 6),
			})
		}
	}

	// Cursor on last entry — matches the common "scrolling at the tail" state.
	m.cursor = len(m.entries) - 1
	return m
}

// BenchmarkBuildContentLines measures the cost of one full content-line
// rebuild for a session with N entries. This is the hot path executed on
// every View() call (and currently a second time per MouseWheelDown via
// clampScroll). Establishing a baseline lets us track whether per-entry
// caching and virtualization land the wins we expect.
func BenchmarkBuildContentLines(b *testing.B) {
	for _, n := range []int{50, 200, 1000} {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			m := makeBenchModel(b, n)
			// Warm any per-entry caches once so we measure the steady-state
			// (post-cache-fill) cost — that's what scrolling actually pays.
			_ = m.buildContentLines()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.buildContentLines()
			}
		})
	}
}

// BenchmarkMouseWheelDown measures the wall cost of a single wheel-down
// event. Today this triggers buildContentLines twice (once via clampScroll,
// once via View). After Phase 1 it should drop to ~one virtualized rebuild.
func BenchmarkMouseWheelDown(b *testing.B) {
	for _, n := range []int{50, 200, 1000} {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			m := makeBenchModel(b, n)
			// Warm cache.
			_ = m.buildContentLines()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.scrollOffset += 3
				_ = m.clampScroll()
				_ = m.buildContentLines() // mirrors the View() rebuild
			}
		})
	}
}

// BenchmarkRenderMarkdown measures glamour rendering cost for the synthetic
// assistant body at terminal width. Used to verify that the per-width
// renderer pool reduces per-call overhead.
func BenchmarkRenderMarkdown(b *testing.B) {
	const body = "Here is **bold** and a list:\n\n- one\n- two\n\n```go\nfunc x() {}\n```\n"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = renderMarkdown(body, 116)
	}
}
