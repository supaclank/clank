package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// A sidebar session row is budgeted at exactly two lines (title + relative
// time), and visibleBodyNodes counts it as two. A title carrying newlines
// (its first prompt had them) must not render extra rows, or the sidebar's
// line accounting drifts (pushing the footer out of the bordered height)
// and mouse-to-row hit-testing desyncs.

func TestSidebar_NewlineTitleRendersTwoLines(t *testing.T) {
	t.Parallel()
	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return now }

	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Title: "first line\nsecond line", Status: agent.StatusIdle, UpdatedAt: now},
	})

	row := strings.Join(m.renderSessionRow(sessionNode{Session: m.sessions[0]}, false, 60), "\n")
	if h := lipgloss.Height(row); h != 2 {
		t.Errorf("sidebar row height=%d for a newline title, want 2 (title + time); "+
			"a multi-row title desyncs the sidebar's line accounting:\n%q", h, row)
	}
}

// The typewriter path bypasses sessionTitle() — it stores its own target
// and renders a revealed prefix — so the flatten must also happen there,
// else a prefix that lands on the newline renders an extra row.
func TestSidebar_NewlineTitleAnimationStaysTwoLines(t *testing.T) {
	t.Parallel()
	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return now }

	// Seed, then change the title to one with a newline to start a
	// typewriter. "alpha" is 5 runes, so the newline sits at index 5.
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "p", Status: agent.StatusIdle, UpdatedAt: now},
	})
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "p", Title: "alpha\nbeta gamma delta", Status: agent.StatusIdle, UpdatedAt: now},
	})
	if !m.hasActiveTitleAnimation("s1") {
		t.Fatal("expected a title animation to be in flight")
	}

	// Reveal past where the newline would be (3 ticks * 2 = 6 runes > 5)
	// while keeping the animation mid-flight.
	m.AdvanceTitleAnimations()
	m.AdvanceTitleAnimations()
	m.AdvanceTitleAnimations()
	if !m.hasActiveTitleAnimation("s1") {
		t.Fatal("animation completed too early; pick a longer title")
	}

	row := strings.Join(m.renderSessionRow(sessionNode{Session: m.sessions[0]}, false, 60), "\n")
	if h := lipgloss.Height(row); h != 2 {
		t.Errorf("sidebar row height=%d during a newline-title animation, want 2:\n%q", h, row)
	}
}
