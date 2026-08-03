package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
)

// TestSidebar_TitleAnimation_StartsOnTitleChange verifies that an
// existing session whose Title changes between two SetSessions calls
// starts a typewriter animation — so the user sees the new title
// being typed rather than instantly swapped in.
func TestSidebar_TitleAnimation_StartsOnTitleChange(t *testing.T) {
	t.Parallel()
	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return now }

	// First call seeds the cache; nothing should animate (no prior value).
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "hello", Status: agent.StatusIdle, UpdatedAt: now},
	})
	if m.hasActiveTitleAnimation("s1") {
		t.Fatalf("first SetSessions should not start an animation")
	}

	// Title arrives → typewriter starts.
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "hello", Title: "Refactor sidebar layout", Status: agent.StatusIdle, UpdatedAt: now},
	})
	if !m.hasActiveTitleAnimation("s1") {
		t.Fatalf("title change should start a typewriter animation")
	}
}

// TestSidebar_TitleAnimation_RevealsCharsOverTime verifies that
// AdvanceTitleAnimations reveals additional runes per tick, and that
// the rendered row shows only the revealed prefix while the animation
// is active. Regression guard: ensures the sidebar redraw uses the
// animated title rather than the full one until completion.
func TestSidebar_TitleAnimation_RevealsCharsOverTime(t *testing.T) {
	t.Parallel()
	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return now }

	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "p", Status: agent.StatusIdle, UpdatedAt: now},
	})
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "p", Title: "ABCDEFGH", Status: agent.StatusIdle, UpdatedAt: now},
	})

	// Initial render: animation just started, only the first frame's
	// chars are revealed (less than full).
	full := "ABCDEFGH"
	initial := strings.Join(m.renderSessionRow(sessionNode{Session: m.sessions[0]}, false, 60), "\n")
	if strings.Contains(initial, full) {
		t.Fatalf("initial render should not yet show full title; got %q", initial)
	}

	// Drive ticks until the animation completes. Each call advances
	// by titleAnimationCharsPerTick runes.
	for i := 0; i < 20 && m.hasActiveTitleAnimation("s1"); i++ {
		m.AdvanceTitleAnimations()
	}

	if m.hasActiveTitleAnimation("s1") {
		t.Fatalf("animation should have completed within 20 ticks")
	}
	final := strings.Join(m.renderSessionRow(sessionNode{Session: m.sessions[0]}, false, 60), "\n")
	if !strings.Contains(final, full) {
		t.Fatalf("final render should show full title; got %q", final)
	}
}

// TestSidebar_TitleAnimation_SameTitleNoOp verifies that re-applying
// SetSessions with an unchanged Title does not (re)start an animation.
// Without this guard every SSE event for an unrelated field (status,
// read state) would retrigger the typewriter.
func TestSidebar_TitleAnimation_SameTitleNoOp(t *testing.T) {
	t.Parallel()
	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return now }

	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Title: "Stable title", Status: agent.StatusIdle, UpdatedAt: now},
	})
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Title: "Stable title", Status: agent.StatusBusy, UpdatedAt: now},
	})
	if m.hasActiveTitleAnimation("s1") {
		t.Fatalf("unchanged title must not start an animation")
	}
}

// TestSidebar_TitleAnimation_FirstLoadNoAnimation verifies that loading
// sessions for the first time (no prior cache) does NOT animate every
// title — that would flood the sidebar with typewriter effects on
// startup. The animation only triggers on genuine in-session changes.
func TestSidebar_TitleAnimation_FirstLoadNoAnimation(t *testing.T) {
	t.Parallel()
	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return now }

	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Title: "Already had a title", Status: agent.StatusIdle, UpdatedAt: now},
		{ID: "s2", Title: "Also titled", Status: agent.StatusIdle, UpdatedAt: now},
	})
	if m.hasActiveTitleAnimation("s1") || m.hasActiveTitleAnimation("s2") {
		t.Fatalf("first load should not animate pre-existing titles")
	}
}

// TestSidebar_TitleAnimation_TitleClearedDropsAnimation verifies that
// when a title transitions from non-empty back to "" (rare, but happens
// on retitle-pending or a forced clear), any in-flight animation is
// dropped. Otherwise renderedTitleFor keeps returning the stale prefix
// of the old animation forever, leaving the row showing text that no
// longer matches the session.
func TestSidebar_TitleAnimation_TitleClearedDropsAnimation(t *testing.T) {
	t.Parallel()
	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return now }

	// Seed with empty, then introduce a title to start an animation.
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "p", Status: agent.StatusIdle, UpdatedAt: now},
	})
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "p", Title: "First title", Status: agent.StatusIdle, UpdatedAt: now},
	})
	if !m.hasActiveTitleAnimation("s1") {
		t.Fatalf("precondition: animation should be active after title set")
	}

	// Title cleared back to "". Animation entry must be dropped so the row
	// doesn't keep rendering the stale prefix.
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "p", Title: "", Status: agent.StatusIdle, UpdatedAt: now},
	})
	if m.hasActiveTitleAnimation("s1") {
		t.Fatalf("animation should be dropped when title is cleared to empty")
	}
	// renderedTitleFor with empty `full` must not resurrect the stale prefix.
	if got := m.renderedTitleFor("s1", ""); got != "" {
		t.Fatalf("renderedTitleFor after clear: got %q, want empty", got)
	}
}

// TestSidebar_TitleAnimation_EmptyToTitleAnimates verifies the most
// common path: session is created with no Title, then the daemon
// generates one and emits EventTitleChange. The empty→non-empty
// transition is exactly when the user benefits most from seeing the
// title appear gradually.
func TestSidebar_TitleAnimation_EmptyToTitleAnimates(t *testing.T) {
	t.Parallel()
	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return now }

	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "do the thing", Status: agent.StatusIdle, UpdatedAt: now},
	})
	m.SetSessions([]agent.SessionInfo{
		{ID: "s1", Prompt: "do the thing", Title: "Do the thing", Status: agent.StatusIdle, UpdatedAt: now},
	})
	if !m.hasActiveTitleAnimation("s1") {
		t.Fatalf("empty→non-empty title should animate")
	}
}
