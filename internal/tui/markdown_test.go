package tui

import (
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

func TestRenderMarkdown_CodeBlock(t *testing.T) {
	t.Parallel()

	md := "Here is code:\n\n```go\nfmt.Println(\"hello\")\n```\n"
	out := renderMarkdown(md, 80)
	plain := ansi.Strip(out)

	if !strings.Contains(plain, "fmt.Println") {
		t.Errorf("rendered markdown should contain code block content, got:\n%s", plain)
	}
	// Glamour should not leave the raw backtick fences in the output.
	if strings.Contains(plain, "```") {
		t.Errorf("rendered markdown should not contain raw backtick fences, got:\n%s", plain)
	}
}

func TestRenderMarkdown_Bold(t *testing.T) {
	t.Parallel()

	md := "This is **bold** text"
	out := renderMarkdown(md, 80)
	plain := ansi.Strip(out)

	if !strings.Contains(plain, "bold") {
		t.Errorf("rendered markdown should contain bold text, got:\n%s", plain)
	}
	// The ** markers should be consumed by the renderer.
	if strings.Contains(plain, "**") {
		t.Errorf("rendered markdown should not contain raw ** markers, got:\n%s", plain)
	}
}

func TestRenderMarkdown_List(t *testing.T) {
	t.Parallel()

	md := "- item one\n- item two\n- item three"
	out := renderMarkdown(md, 80)
	plain := ansi.Strip(out)

	if !strings.Contains(plain, "item one") {
		t.Errorf("rendered markdown should contain list items, got:\n%s", plain)
	}
}

func TestRenderMarkdown_FallbackOnZeroWidth(t *testing.T) {
	t.Parallel()

	md := "hello world"
	// Width 0 triggers wrapText fallback inside renderMarkdown if the renderer
	// creation fails or produces empty output. Either way it should not panic.
	out := renderMarkdown(md, 0)
	if out == "" {
		t.Error("renderMarkdown with width=0 should still produce output")
	}
}

func TestRenderEntry_MarkdownWhenIdle(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.info = &agent.SessionInfo{Status: agent.StatusIdle}
	e := displayEntry{kind: entryText, content: "This is **bold** text"}

	lines := m.renderEntry(&e, false)
	joined := strings.Join(lines, "\n")
	plain := ansi.Strip(joined)

	// When idle, renderEntry should use markdown rendering.
	// The ** markers should be consumed.
	if strings.Contains(plain, "**") {
		t.Errorf("idle renderEntry should render markdown (no raw **), got:\n%s", plain)
	}
}

func TestRenderEntry_PlainTextWhenBusy(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.info = &agent.SessionInfo{Status: agent.StatusBusy}
	e := displayEntry{kind: entryText, content: "This is **bold** text"}

	lines := m.renderEntry(&e, false)
	joined := strings.Join(lines, "\n")
	plain := ansi.Strip(joined)

	// When busy (streaming), renderEntry should use plain wrapText.
	// The ** markers should still be present in the raw output.
	if !strings.Contains(plain, "**bold**") {
		t.Errorf("busy renderEntry should show raw markdown, got:\n%s", plain)
	}
}

func TestRenderEntry_MarkdownCacheReused(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.info = &agent.SessionInfo{Status: agent.StatusIdle}
	e := displayEntry{kind: entryText, content: "Hello **world**"}

	// First render populates the cache.
	lines1 := m.renderEntry(&e, false)
	if e.renderedMD == "" {
		t.Fatal("renderedMD should be populated after first render")
	}
	if e.renderedWidth == 0 {
		t.Fatal("renderedWidth should be set after first render")
	}
	cachedMD := e.renderedMD

	// Second render should reuse the cache (same width, same content).
	lines2 := m.renderEntry(&e, false)
	if e.renderedMD != cachedMD {
		t.Error("renderedMD should not change on second render with same width")
	}
	if len(lines1) != len(lines2) {
		t.Errorf("cached render should produce same line count: %d vs %d", len(lines1), len(lines2))
	}
}

func TestRenderEntry_MarkdownCacheInvalidatedOnContentChange(t *testing.T) {
	t.Parallel()

	m := newTestSessionModel(nil)
	m.info = &agent.SessionInfo{Status: agent.StatusIdle}
	e := displayEntry{kind: entryText, content: "Hello **world**"}

	// Populate cache.
	m.renderEntry(&e, false)
	oldCache := e.renderedMD

	// Simulate content change (like upsertPartEntry does).
	e.content = "Hello **universe**"
	e.renderedMD = "" // invalidate

	m.renderEntry(&e, false)
	if e.renderedMD == oldCache {
		t.Error("renderedMD should change after content update")
	}
	plain := ansi.Strip(e.renderedMD)
	if !strings.Contains(plain, "universe") {
		t.Errorf("re-rendered markdown should contain new content, got:\n%s", plain)
	}
}

func TestRenderEntry_MarkdownPreservesSelectionHighlight(t *testing.T) {
	t.Parallel()

	// Verify that markdown-rendered content still works with highlightLine.
	m := newTestSessionModel(nil)
	m.info = &agent.SessionInfo{Status: agent.StatusIdle}
	e := displayEntry{kind: entryText, content: "This is **bold** text with `code`"}

	lines := m.renderEntry(&e, false)
	// Simulate selection across the rendered lines.
	sel := &textSelection{active: true, startX: 0, startY: 0, endX: 20, endY: 0}
	for _, line := range lines {
		// highlightLine should not panic on glamour output.
		highlighted := sel.highlightLine(line, 0)
		if highlighted == "" && line != "" {
			t.Error("highlightLine should not return empty for non-empty input")
		}
	}
}

func TestRenderEntry_MarkdownExtractAfterStrip(t *testing.T) {
	t.Parallel()

	// Verify that Extract produces clean text from markdown-rendered content.
	m := newTestSessionModel(nil)
	m.info = &agent.SessionInfo{Status: agent.StatusIdle}
	e := displayEntry{kind: entryText, content: "Hello **world**"}

	lines := m.renderEntry(&e, false)

	sel := &textSelection{
		active: true,
		startX: 0, startY: 0,
		endX: 80, endY: len(lines) - 1,
	}
	text := sel.Extract(lines, 0, 0)
	if !strings.Contains(text, "world") {
		t.Errorf("Extract from markdown-rendered lines should contain 'world', got: %q", text)
	}
	// After ANSI stripping, there should be no raw markdown markers.
	if strings.Contains(text, "**") {
		t.Errorf("Extract should not contain raw ** markers, got: %q", text)
	}
}
