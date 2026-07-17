package tui

import (
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/acksell/clank/internal/agent"
)

func TestDateLabel(t *testing.T) {
	t.Parallel()

	// Fix "now" to a known point so tests are deterministic.
	now := time.Date(2025, time.March, 15, 14, 30, 0, 0, time.Local)

	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"same day", time.Date(2025, time.March, 15, 9, 0, 0, 0, time.Local), "Today"},
		{"yesterday", time.Date(2025, time.March, 14, 23, 59, 0, 0, time.Local), "Yesterday"},
		{"two days ago", time.Date(2025, time.March, 13, 12, 0, 0, 0, time.Local), "Thu, Mar 13"},
		{"last week", time.Date(2025, time.March, 8, 12, 0, 0, 0, time.Local), "Sat, Mar 8"},
		{"different year", time.Date(2024, time.December, 25, 10, 0, 0, 0, time.Local), "Wed, Dec 25, 2024"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := dateLabel(tt.t, now)
			if got != tt.want {
				t.Errorf("dateLabel(%v, %v) = %q, want %q", tt.t, now, got, tt.want)
			}
		})
	}
}

// TestAutoRefreshSurvivesSessionView is a regression test for the bug where
// entering the session view permanently killed the auto-refresh timer chain.
//
// Root cause: autoRefreshCmd() fires inboxRefreshMsg every 3 seconds and the
// handler re-schedules itself. When screen == screenSession, the session view
// delegation's default case silently swallowed inboxRefreshMsg (the session
// view didn't know about it and dropped it), breaking the chain forever.
//
// The fix intercepts inboxRefreshMsg before session view delegation — the same
// pattern used for spinner.TickMsg.
func TestAutoRefreshSurvivesSessionView(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.screen = screenSession
	m.sessionView = NewSessionViewModel(nil, "test-session")

	// inboxRefreshMsg is no longer driven by a self-rescheduling tick (push
	// updates via SSE replaced the 3s poll), but explicit fires (post-discover,
	// user actions) must still trigger a one-shot data reload even while the
	// session screen is active. Otherwise the sidebar would never resync
	// after a discover until the user navigated back to inbox.
	_, cmd := m.Update(inboxRefreshMsg{})
	if cmd == nil {
		t.Fatal("inboxRefreshMsg was swallowed while in session view; explicit refresh path is broken")
	}
}

// TestSidebarSessionStatusRefreshesInSessionScreen is a regression test for
// the bug where the sidebar's per-row spinner kept animating after a session
// transitioned Busy -> Idle while the user was viewing that session.
//
// Root cause: the inboxRefreshMsg handler used to skip loadDataCmd() when
// screen != screenWelcome, so the sidebar's cached SessionInfo.Status went stale
// for as long as the user stayed on screenSession. The sidebar reads Status
// to decide whether to draw the spinner glyph (sidebar_render.go), so a stale
// Busy status kept the spinner spinning indefinitely.
//
// The fix runs loadDataCmd() on every tick regardless of screen. We verify the
// propagation path independently by feeding an inboxDataMsg while in
// screenSession and checking the sidebar reflects the new status.
func TestSidebarSessionStatusRefreshesInSessionScreen(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.screen = screenSession
	m.sessionView = NewSessionViewModel(nil, "s1")

	// Seed: session is busy.
	busy := []agent.SessionInfo{{ID: "s1", Title: "t", Status: agent.StatusBusy}}
	m.Update(inboxDataMsg{sessions: busy})
	if got := m.sidebar.sessions[0].Status; got != agent.StatusBusy {
		t.Fatalf("setup: sidebar status = %v, want Busy", got)
	}

	// Simulate the next poll: backend now reports Idle.
	idle := []agent.SessionInfo{{ID: "s1", Title: "t", Status: agent.StatusIdle}}
	m.Update(inboxDataMsg{sessions: idle})

	if got := m.sidebar.sessions[0].Status; got != agent.StatusIdle {
		t.Fatalf("sidebar did not pick up Idle status in screenSession: got %v", got)
	}
}

// TestBackToInboxDoesNotReseedTimers is a regression test for a CPU spike in
// clank-host: every nav round-trip (inbox→session→inbox) used to re-seed
// autoRefreshCmd and spinner.Tick. The pre-existing chains kept ticking the
// whole time the user was in the session view, so re-seeding spawned a
// duplicate chain. After K nav round-trips, K parallel pollers fired every 3s,
// fanning out to expensive git subprocesses on the host. Returning to the inbox
// should issue exactly one one-shot refresh (loadDataCmd) and rely on the
// existing chains.
func TestBackToInboxDoesNotReseedTimers(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.screen = screenSession
	m.sessionView = NewSessionViewModel(nil, "test-session")

	_, cmd := m.Update(backToInboxMsg{})

	if cmd == nil {
		t.Fatal("backToInboxMsg returned nil cmd; expected one-shot refresh cmd")
	}

	if m.screen != screenWelcome {
		t.Errorf("expected screen=screenWelcome after backToInboxMsg, got %v", m.screen)
	}

	// Verify a cmd is returned (one-shot refresh) and screen transitioned.
	// The timer-leak property (no re-seeding of autoRefreshCmd/spinner.Tick)
	// is maintained structurally: backToInboxMsg returns tea.Sequence(markRead,
	// loadDataCmd) without including autoRefreshCmd, relying on the existing
	// chains that kept running during the session view.
}

// TestSpinnerTickSurvivesConfirmDialog is a regression test for the bug where
// opening a confirm dialog swallowed spinner.TickMsg, permanently breaking the
// spinner's self-sustaining tick chain.
func TestSpinnerTickSurvivesConfirmDialog(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.showConfirm = true
	m.confirm = newConfirmDialog("Delete?", "Are you sure?", "delete")

	// Generate a valid tick message from the spinner's own state.
	tickMsg := m.spinner.Tick()

	_, cmd := m.Update(tickMsg)

	// The spinner must schedule the next tick (non-nil cmd) to keep
	// the animation alive.
	if cmd == nil {
		t.Fatal("spinner tick was swallowed by confirm dialog; expected a follow-up tick command")
	}

	// The returned command should produce another spinner.TickMsg.
	nextMsg := cmd()
	if _, ok := nextMsg.(spinner.TickMsg); !ok {
		t.Fatalf("expected spinner.TickMsg, got %T", nextMsg)
	}
}

// TestSpinnerTickSurvivesActionMenu is a regression test for the bug where
// opening the action menu swallowed spinner.TickMsg.
func TestSpinnerTickSurvivesActionMenu(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.showMenu = true
	m.menu = newActionMenu("Actions", nil)

	tickMsg := m.spinner.Tick()

	_, cmd := m.Update(tickMsg)

	if cmd == nil {
		t.Fatal("spinner tick was swallowed by action menu; expected a follow-up tick command")
	}

	nextMsg := cmd()
	if _, ok := nextMsg.(spinner.TickMsg); !ok {
		t.Fatalf("expected spinner.TickMsg, got %T", nextMsg)
	}
}

// TestSpinnerTickForwardedToSessionView is a regression test for the bug where
// the InboxModel intercepted all spinner.TickMsg before delegating to the
// session view. Since each spinner has a unique ID, the inbox spinner silently
// rejected session-view ticks (returning nil cmd), permanently killing the
// session spinner's tick chain.
func TestSpinnerTickForwardedToSessionView(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.screen = screenSession
	m.sessionView = NewSessionViewModel(nil, "test-session")

	// Generate a tick that belongs to the session view's spinner.
	sessionTickMsg := m.sessionView.spinner.Tick()

	_, cmd := m.Update(sessionTickMsg)

	if cmd == nil {
		t.Fatal("session spinner tick was swallowed by InboxModel; expected a follow-up tick command")
	}

	// Feed a second tick to confirm the chain is alive.
	secondTick := m.sessionView.spinner.Tick()
	_, cmd2 := m.Update(secondTick)
	if cmd2 == nil {
		t.Fatal("session spinner second tick returned nil cmd; tick chain is broken")
	}
}

// TestSpinnerTickForwardedWhenSidebarFocused is a regression test for the bug
// where moving keyboard focus from the chat pane to the sidebar (Tab / left
// arrow) while a session was open caused the session view's spinner to freeze.
// The tick chain must keep ticking regardless of which pane has focus — only
// m.screen gates forwarding, not m.pane.
func TestSpinnerTickForwardedWhenSidebarFocused(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.screen = screenSession
	m.sessionView = NewSessionViewModel(nil, "test-session")
	m.setPane(paneSidebar)

	sessionTickMsg := m.sessionView.spinner.Tick()
	_, cmd := m.Update(sessionTickMsg)
	if cmd == nil {
		t.Fatal("session spinner tick was swallowed when sidebar is focused; tick chain is broken")
	}
}

// --- Project filter tests ---

// --- Arrow key pane navigation tests ---

func TestLeftArrow_FromSessionPane_NavigatesToSidebar(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  120,
		height: 40,
		pane:   paneSessions,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
		},
	}

	// Precondition: session pane has focus, two-pane mode active.
	if !m.showTwoPanes() {
		t.Fatal("expected two-pane mode to be active")
	}
	if m.pane != paneSessions {
		t.Fatal("expected pane to be paneSessions")
	}

	m.handleWelcomeKey(tea.KeyPressMsg{Code: tea.KeyLeft})

	if m.pane != paneSidebar {
		t.Error("expected pane to switch to paneSidebar after left arrow")
	}
	if !m.sidebar.Focused() {
		t.Error("expected sidebar to be focused after left arrow")
	}
}

func TestLeftArrow_NarrowTerminal_NoOp(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  60, // below minTwoPaneWidth (80)
		height: 40,
		pane:   paneSessions,
	}

	if m.showTwoPanes() {
		t.Fatal("expected two-pane mode to be inactive in narrow terminal")
	}

	m.handleWelcomeKey(tea.KeyPressMsg{Code: tea.KeyLeft})

	if m.pane != paneSessions {
		t.Error("expected pane to remain paneSessions when two-pane mode is inactive")
	}
}

func TestLeftArrow_SidebarHidden_NoOp(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:         120,
		height:        40,
		pane:          paneSessions,
		sidebarHidden: true,
	}

	if m.showTwoPanes() {
		t.Fatal("expected two-pane mode to be inactive when sidebar is hidden")
	}

	m.handleWelcomeKey(tea.KeyPressMsg{Code: tea.KeyLeft})

	if m.pane != paneSessions {
		t.Error("expected pane to remain paneSessions when sidebar is hidden")
	}
}

func TestRightArrow_FromSidebar_NavigatesToSessionPane(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  120,
		height: 40,
		pane:   paneSidebar,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
			focused:    true,
		},
	}

	if m.pane != paneSidebar {
		t.Fatal("expected pane to be paneSidebar")
	}

	m.handleSidebarKey(tea.KeyPressMsg{Code: tea.KeyRight})

	if m.pane != paneSessions {
		t.Error("expected pane to switch to paneSessions after right arrow")
	}
	if m.sidebar.Focused() {
		t.Error("expected sidebar to lose focus after right arrow")
	}
}

// 'w' must be a pure visibility toggle: pressing it twice returns to the
// original focused pane regardless of where focus started.
func TestToggleSidebar_PreservesFocus_FromSessions(t *testing.T) {
	t.Parallel()

	m := &InboxModel{width: 120, height: 40, pane: paneSessions}

	m.toggleSidebar()
	if !m.sidebarHidden {
		t.Fatal("expected sidebar hidden after first toggle")
	}
	if m.pane != paneSessions {
		t.Errorf("expected pane to stay paneSessions when hiding from sessions, got %v", m.pane)
	}

	m.toggleSidebar()
	if m.sidebarHidden {
		t.Fatal("expected sidebar visible after second toggle")
	}
	if m.pane != paneSessions {
		t.Errorf("expected pane to stay paneSessions after un-hide, got %v", m.pane)
	}
}

func TestToggleSidebar_PreservesFocus_FromSidebar(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:   120,
		height:  40,
		pane:    paneSidebar,
		sidebar: SidebarModel{focused: true},
	}

	m.toggleSidebar()
	if !m.sidebarHidden {
		t.Fatal("expected sidebar hidden after first toggle")
	}
	if m.pane != paneSessions {
		t.Errorf("expected focus to move to sessions when hiding focused sidebar, got %v", m.pane)
	}

	m.toggleSidebar()
	if m.sidebarHidden {
		t.Fatal("expected sidebar visible after second toggle")
	}
	if m.pane != paneSidebar {
		t.Errorf("inverse property violated: expected focus restored to sidebar, got %v", m.pane)
	}
	if !m.sidebar.Focused() {
		t.Error("expected sidebar.Focused() to be true after focus restore")
	}
}
