package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// The sidebar collapses the unread marker and the agent-status spinner
// into a single indicator column. Spinner wins over the marker so an
// actively-working session shows it's working. Unread rows bold the
// title; the same bold style is used on hover (matching other sidebar
// sections like the "All sessions" row) — see the dedicated hover test
// below.
func TestRenderSessionRow_Indicator(t *testing.T) {
	t.Parallel()

	const spinnerFrame = "⠋"
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

	// unreadAt makes a session's LastReadAt older than UpdatedAt so
	// SessionInfo.Unread() returns true.
	unreadAt := now.Add(-time.Hour)

	type want struct {
		hasSpinner bool
		hasUnread  bool // the "*" glyph
		hasFollow  bool // the "!" glyph
		titleBold  bool // textColor + bold (unread, non-selected)
		titlePlain bool // textColor, no bold (read, non-selected)
	}
	cases := []struct {
		name    string
		session agent.SessionInfo
		want    want
	}{
		{
			name:    "idle + unread shows asterisk and bold title",
			session: agent.SessionInfo{ID: "s", Title: "t", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: unreadAt},
			want:    want{hasUnread: true, titleBold: true},
		},
		{
			name:    "busy + read shows spinner and plain title",
			session: agent.SessionInfo{ID: "s", Title: "t", Status: agent.StatusBusy, UpdatedAt: now, LastReadAt: now},
			want:    want{hasSpinner: true, titlePlain: true},
		},
		{
			name:    "busy + unread shows spinner and keeps bold title",
			session: agent.SessionInfo{ID: "s", Title: "t", Status: agent.StatusBusy, UpdatedAt: now, LastReadAt: unreadAt},
			want:    want{hasSpinner: true, titleBold: true},
		},
		{
			name:    "follow-up beats unread asterisk",
			session: agent.SessionInfo{ID: "s", Title: "t", Status: agent.StatusIdle, FollowUp: true, UpdatedAt: now, LastReadAt: unreadAt},
			want:    want{hasFollow: true, titleBold: true},
		},
		{
			name:    "idle + read renders quiet",
			session: agent.SessionInfo{ID: "s", Title: "t", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
			want:    want{titlePlain: true},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
			m.SetCloudSpinnerFrame(spinnerFrame)

			lines := m.renderSessionRow(sessionNode{Session: tc.session}, false, 60)
			if len(lines) == 0 {
				t.Fatalf("renderSessionRow returned no lines")
			}
			line := lines[0]

			if got := strings.Contains(line, spinnerFrame); got != tc.want.hasSpinner {
				t.Errorf("spinner: got %v, want %v (line=%q)", got, tc.want.hasSpinner, line)
			}
			if got := strings.Contains(line, "*"); got != tc.want.hasUnread {
				t.Errorf("unread *: got %v, want %v (line=%q)", got, tc.want.hasUnread, line)
			}
			if got := strings.Contains(line, "!"); got != tc.want.hasFollow {
				t.Errorf("follow-up !: got %v, want %v (line=%q)", got, tc.want.hasFollow, line)
			}
			// Reconstruct expected lipgloss-rendered prefixes so we
			// pin the exact ANSI for each title style and avoid
			// false-positives from earlier glyphs on the line.
			boldTitle := lipgloss.NewStyle().Foreground(textColor).Bold(true).Render("t")
			plainTitle := lipgloss.NewStyle().Foreground(textColor).Render("t")
			if got := strings.Contains(line, boldTitle); got != tc.want.titleBold {
				t.Errorf("bold title: got %v, want %v (line=%q)", got, tc.want.titleBold, line)
			}
			// Plain check requires the bold variant be absent —
			// boldTitle is a superset substring of plainTitle ANSI.
			gotPlain := strings.Contains(line, plainTitle) && !strings.Contains(line, boldTitle)
			if gotPlain != tc.want.titlePlain {
				t.Errorf("plain title: got %v, want %v (line=%q)", gotPlain, tc.want.titlePlain, line)
			}
		})
	}
}

// Hovered (cursor) rows bold the title — mirrors the behaviour of
// other sidebar sections (e.g. worktree rows). No color change; bold is
// the only emphasis.
func TestRenderSessionRow_HoveredIsBold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	session := agent.SessionInfo{ID: "s", Title: "t", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now}

	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	lines := m.renderSessionRow(sessionNode{Session: session}, true, 60)
	if len(lines) == 0 {
		t.Fatalf("renderSessionRow returned no lines")
	}
	wantBold := lipgloss.NewStyle().Foreground(textColor).Bold(true).Render("t")
	if !strings.Contains(lines[0], wantBold) {
		t.Errorf("hovered title %q missing bold textColor segment %q", lines[0], wantBold)
	}
}

// Regression: the active session's rail used to be concatenated INSIDE
// the dimColor lipgloss wrap on the age line, so its trailing SGR reset
// killed the dim foreground on the rendered time text — making the
// active row's age display in the terminal default colour instead of
// dim. The fix concatenates the rail outside the styled segment. This
// test pins the styling so a future refactor can't silently regress it.
func TestRenderSessionRow_AgeLineColor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	session := agent.SessionInfo{ID: "s", Title: "t", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now}

	// The renderer wraps indicator-width padding + the time text in a
	// single styled segment. Reconstruct the expected wrapped substring
	// so the test pins both the colour AND that rail concatenation
	// hasn't broken the SGR span (the original regression).
	const indicatorPad = "  " // matches indicatorWidth in renderSessionRow
	wantTime := lipgloss.NewStyle().Foreground(dimColor).
		Render(indicatorPad + shortTimeAgo(now))

	cases := []struct {
		name     string
		selected bool
		active   bool
	}{
		{name: "unselected, inactive"},
		{name: "selected, inactive", selected: true},
		{name: "unselected, active", active: true},
		{name: "selected, active", selected: true, active: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
			if tc.active {
				m.SetActiveSessionID(session.ID)
			}
			lines := m.renderSessionRow(sessionNode{Session: session}, tc.selected, 60)
			if len(lines) != 2 {
				t.Fatalf("want 2 lines, got %d", len(lines))
			}
			ageLine := lines[1]

			if !strings.Contains(ageLine, wantTime) {
				t.Errorf("age line %q missing wrapped dim time %q", ageLine, wantTime)
			}
		})
	}
}

// While the welcome screen is active, the Home row shows the left rail
// (▎) to mark "what's open". The right-edge cursor chevron tracks the
// keyboard cursor only, so an unfocused sidebar (cursor elsewhere) must
// NOT show the chevron on Home even while active.
func TestRenderHomeRow_RailWhenUnfocusedActive(t *testing.T) {
	t.Parallel()

	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	m.SetFocused(false)

	inactive := m.renderHomeRow(false, 60)
	if strings.Contains(inactive, activeSessionRail) {
		t.Fatalf("home-inactive row unexpectedly shows rail: %q", inactive)
	}

	m.SetHomeActive(true)
	active := m.renderHomeRow(false, 60)
	if !strings.Contains(active, activeSessionRail) {
		t.Errorf("home-active unfocused row missing rail %q: %q", activeSessionRail, active)
	}
	if strings.Contains(active, rightCursorGlyph) {
		t.Errorf("home-active unfocused row should not show cursor chevron (cursor is elsewhere): %q", active)
	}
}

// The ▎ rail is a single shared "what's open" marker. If a session is
// active, Home must not also paint the rail — otherwise the rail shows
// on two rows at once.
func TestRenderHomeRow_RailSuppressedWhenSessionActive(t *testing.T) {
	t.Parallel()

	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	m.SetFocused(false)
	m.SetHomeActive(true)
	m.SetActiveSessionID("sess-1")

	row := m.renderHomeRow(false, 60)
	if strings.Contains(row, activeSessionRail) {
		t.Errorf("Home rail rendered while a session is active: %q", row)
	}
}

// The chevron tracks the keyboard cursor: it must disappear from Home
// once the cursor moves away, even while the welcome screen stays
// active (rail persists, chevron does not).
func TestRenderHomeRow_ChevronTracksCursorNotActive(t *testing.T) {
	t.Parallel()

	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	m.SetFocused(true)
	m.SetHomeActive(true)

	// Cursor moved off Home (selected=false) while still focused.
	row := m.renderHomeRow(false, 60)
	if strings.Contains(row, rightCursorGlyph) {
		t.Errorf("chevron lingered on Home after cursor moved away: %q", row)
	}
	if !strings.Contains(row, activeSessionRail) {
		t.Errorf("rail should persist while welcome screen active: %q", row)
	}
}

// The focused cursor on Home shows the same right-edge chevron as
// worktree rows.
func TestRenderHomeRow_ChevronWhenFocused(t *testing.T) {
	t.Parallel()

	m := NewSidebarModel(nil, "host", agent.GitRef{}, "/r")
	m.SetFocused(true)

	row := m.renderHomeRow(true, 60)
	if !strings.Contains(row, rightCursorGlyph) {
		t.Errorf("focused+selected home row missing chevron %q: %q", rightCursorGlyph, row)
	}
}
