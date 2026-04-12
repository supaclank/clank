package tui

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemon"
)

// inboxScreen tracks which screen is active within the inbox app.
type inboxScreen int

const (
	screenInbox   inboxScreen = iota
	screenSession             // Viewing a specific session (or composing a new one)
)

// inboxPane tracks which pane has keyboard focus in the two-pane layout.
type inboxPane int

const (
	paneSessions inboxPane = iota // Right pane: session list (default)
	paneBranches                  // Left pane: branch list
)

// inboxRefreshMsg triggers a data reload from the daemon.
type inboxRefreshMsg struct{}

// inboxRow is one selectable row in the inbox.
type inboxRow struct {
	session       *agent.SessionInfo // nil for non-session rows (e.g. accordion)
	accordion     string             // non-empty = archive accordion for this date group label
	doneAccordion string             // non-empty = done accordion for this date group label
}

// inboxGroup is a named section of rows.
type inboxGroup struct {
	name         string
	style        lipgloss.Style
	rows         []inboxRow // active (non-done, non-archived) rows
	doneRows     []inboxRow // done rows shown when accordion is expanded
	archivedRows []inboxRow // archived rows shown when accordion is expanded
}

// InboxModel is the top-level Bubble Tea model for the agent inbox.
// It uses a two-pane layout: left pane shows branches, right pane shows
// sessions. In narrow terminals, only the session pane is shown.
type InboxModel struct {
	client *daemon.Client

	// Two-pane layout state.
	pane             inboxPane       // which pane has keyboard focus
	branchPane       BranchPaneModel // left pane: branch list
	branchPaneHidden bool            // true when user toggled sidebar off with 'w'

	// Inbox list state (right pane).
	groups       []inboxGroup
	flatRows     []inboxRow
	cursor       int
	scrollOffset int
	showMenu     bool
	menu         actionMenuModel

	// Archive accordion state — tracks which date groups have their archive expanded.
	archiveExpanded map[string]bool // keyed by date group label

	// Done accordion state — tracks which date groups have their done section expanded.
	doneExpanded map[string]bool // keyed by date group label

	// Confirm dialog state.
	showConfirm bool
	confirm     confirmDialogModel

	// Help overlay state.
	showHelp bool

	// Merge overlay state.
	showMerge    bool
	mergeOverlay mergeOverlayModel

	// Search state.
	searching      bool                // true when the search bar is active
	searchInput    textinput.Model     // text input for search queries
	searchQuery    string              // last query sent to the daemon
	cachedSessions []agent.SessionInfo // last full session list from the daemon

	// Filter state — structured filters applied as pills in the search bar.
	projectDir    string // absolute path of the cwd when the inbox was launched
	projectName   string // basename of projectDir, used for display
	projectFilter bool   // when true, only show sessions matching projectDir

	// Pre-built display data.
	displayLines []string
	rowToLine    []int

	// Session detail sub-view.
	screen       inboxScreen
	sessionView  *SessionViewModel
	activeConnID string // session ID of the detail view

	// Spinner for busy session indicators.
	spinner spinner.Model

	// Voice state — persists across inbox/session navigation.
	voice voiceState

	// kittyKeyboard is true when the terminal supports the Kitty keyboard
	// protocol (specifically ReportEventTypes, which delivers KeyReleaseMsg).
	// Set once via KeyboardEnhancementsMsg during startup. Push-to-talk
	// requires this; without it we show a warning popup instead.
	kittyKeyboard bool

	// showKittyWarning is true when the Kitty keyboard warning popup is visible.
	showKittyWarning bool

	width  int
	height int
	err    error
}

// NewInboxModel creates the inbox TUI connected to the given daemon client.
func NewInboxModel(client *daemon.Client) *InboxModel {
	sp := spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(successColor)),
	)
	ti := textinput.New()
	ti.Placeholder = "Search sessions..."
	ti.CharLimit = 256
	ti.Prompt = "/ "
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(primaryColor).Bold(true)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	styles.Blurred.Prompt = lipgloss.NewStyle().Foreground(dimColor)
	styles.Blurred.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Blurred.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)

	cwd, _ := os.Getwd()
	bp := NewBranchPaneModel(client, cwd)
	return &InboxModel{
		client:      client,
		pane:        paneSessions,
		branchPane:  bp,
		spinner:     sp,
		searchInput: ti,
		projectDir:  cwd,
		projectName: filepath.Base(cwd),
	}
}

func (m *InboxModel) Init() tea.Cmd {
	cmds := []tea.Cmd{func() tea.Msg { return tea.RequestWindowSize }, m.discoverCmd(), m.loadDataCmd(), m.autoRefreshCmd(), m.spinner.Tick, m.branchPane.Init()}
	if m.screen == screenSession && m.sessionView != nil {
		cmds = append(cmds, m.sessionView.Init())
	}
	return tea.Batch(cmds...)
}

// discoverCmd asks the daemon to discover historical sessions from the
// OpenCode backend. Runs asynchronously; when done it triggers a refresh
// so newly-discovered sessions appear in the inbox.
func (m *InboxModel) discoverCmd() tea.Cmd {
	return func() tea.Msg {
		cwd, err := os.Getwd()
		if err != nil {
			return nil // Non-fatal: discovery is best-effort
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = m.client.DiscoverSessions(ctx, cwd)
		// After discovery completes, trigger a refresh to show new sessions.
		return inboxRefreshMsg{}
	}
}

// loadDataCmd fetches sessions from the daemon.
func (m *InboxModel) loadDataCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		sessions, err := m.client.ListSessions(ctx)
		if err != nil {
			return inboxDataMsg{err: err}
		}
		return inboxDataMsg{sessions: sessions}
	}
}

// inboxDataMsg carries fetched session data.
type inboxDataMsg struct {
	sessions []agent.SessionInfo
	err      error
}

// inboxSearchResultMsg carries search results from the daemon.
type inboxSearchResultMsg struct {
	query    string // the query that produced these results
	sessions []agent.SessionInfo
	err      error
}

// autoRefreshCmd schedules periodic data refresh.
func (m *InboxModel) autoRefreshCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return inboxRefreshMsg{}
	})
}

// searchCmd performs a case-insensitive substring search against the daemon.
// Supports pipe-separated OR groups with space-separated AND terms.
func (m *InboxModel) searchCmd(query string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		sessions, err := m.client.SearchSessions(ctx, agent.SearchParams{Query: query})
		return inboxSearchResultMsg{query: query, sessions: sessions, err: err}
	}
}

func (m *InboxModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Always keep the inbox spinner ticking, regardless of modal/screen state.
	// The spinner's tick chain is self-sustaining: each Update schedules
	// the next tick. Swallowing a single TickMsg permanently kills it.
	//
	// When the session view is active its spinner also uses TickMsg. Each
	// spinner has a unique ID so Update returns nil for ticks it doesn't
	// own — but we must still forward the message so the session spinner
	// can process its own ticks.
	if tickMsg, ok := msg.(spinner.TickMsg); ok {
		var inboxCmd tea.Cmd
		m.spinner, inboxCmd = m.spinner.Update(tickMsg)

		// Forward to session view so its spinner keeps ticking too.
		if m.screen == screenSession && m.sessionView != nil {
			model, sessionCmd := m.sessionView.Update(tickMsg)
			m.sessionView = model.(*SessionViewModel)
			return m, tea.Batch(inboxCmd, sessionCmd)
		}
		return m, inboxCmd
	}

	// Always keep the refresh timer ticking, regardless of screen state.
	// Same rationale as the spinner tick chain above: the timer is
	// self-sustaining (each handler schedules the next tick). Letting it
	// fall through to the session view delegation would swallow it and
	// permanently kill the refresh loop.
	if _, ok := msg.(inboxRefreshMsg); ok {
		if m.screen == screenInbox {
			return m, tea.Batch(m.loadDataCmd(), m.branchPane.loadBranches(), m.autoRefreshCmd())
		}
		return m, m.autoRefreshCmd()
	}

	// Detect Kitty keyboard protocol support. Bubble Tea sends this once
	// after startup when the View requests ReportEventTypes.
	if msg, ok := msg.(tea.KeyboardEnhancementsMsg); ok {
		m.kittyKeyboard = msg.SupportsEventTypes()
		return m, nil
	}

	// Voice messages are handled at the inbox level regardless of screen,
	// since voice state persists across navigation.
	if handled, model, cmd := m.handleVoiceMsg(msg); handled {
		return model, cmd
	}

	// Dismiss the Kitty keyboard warning popup on any key press.
	if m.showKittyWarning {
		if _, ok := msg.(tea.KeyPressMsg); ok {
			m.showKittyWarning = false
			return m, nil
		}
	}

	// Push-to-talk: intercept SPACE press/release before any screen-specific
	// handling so voice works on both inbox and session screens.
	// Skip when the branch pane is in text-input mode (creating a new branch)
	// so that space goes to the text input instead.
	voiceInterceptOK := !(m.pane == paneBranches && m.branchPane.creating)
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if voiceInterceptOK {
			if handled, cmd := m.handleVoiceKeyPress(msg); handled {
				return m, cmd
			}
		}
	case tea.KeyReleaseMsg:
		if voiceInterceptOK {
			if handled, cmd := m.handleVoiceKeyRelease(msg); handled {
				return m, cmd
			}
		}
	case sessionEventMsg:
		// Intercept voice SSE events before delegating to session view.
		m.handleVoiceSSE(msg)
		// Don't return — let the event continue to the session view for
		// non-voice handling (or be ignored if it was voice-only).
	}

	// If we're in session detail view (or composing), delegate.
	if m.screen == screenSession && m.sessionView != nil {
		return m.updateSessionView(msg)
	}

	// If help overlay is open, dismiss on any key press.
	if m.showHelp {
		if _, ok := msg.(tea.KeyPressMsg); ok {
			m.showHelp = false
			return m, nil
		}
	}

	// If confirm dialog is open, delegate.
	if m.showConfirm {
		return m.updateConfirm(msg)
	}

	// If merge overlay is open, delegate.
	if m.showMerge {
		return m.updateMerge(msg)
	}

	// If menu is open, delegate to menu.
	if m.showMenu {
		return m.updateMenu(msg)
	}

	// If searching, delegate keyboard input to search handler.
	if m.searching {
		return m.updateSearch(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.searchInput.SetWidth(m.sessionPaneWidth())
		m.branchPane.SetSize(m.branchPaneRenderWidth(), m.height)
		if m.showMerge {
			m.mergeOverlay.SetSize(m.width, m.height)
		}
		return m, nil

	case branchLoadedMsg, branchWorktreeCreatedMsg:
		cmd := m.branchPane.Update(msg)
		return m, cmd

	case inboxDataMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.err = nil
			m.cachedSessions = msg.sessions
			m.updateBranchSessionCounts()
			m.buildGroups(m.filteredSessions())
		}
		return m, nil

	case inboxSearchResultMsg:
		// Late-arriving search result after exiting search mode — ignore.
		return m, nil

	case nativeCLIReturnMsg:
		// User returned from native CLI — refresh inbox to pick up any
		// changes made in the external session.
		if msg.err != nil {
			m.err = msg.err
		}
		return m, m.loadDataCmd()

	case tea.KeyPressMsg:
		// Tab/Shift+Tab switch panes (only in two-pane mode).
		if m.showTwoPanes() {
			if key.Matches(msg, key.NewBinding(key.WithKeys("tab"))) {
				prevBranch := m.branchPane.SelectedBranch()
				if m.pane == paneSessions {
					m.pane = paneBranches
					m.branchPane.SetFocused(true)
				} else {
					m.pane = paneSessions
					m.branchPane.SetFocused(false)
				}
				if m.branchPane.SelectedBranch() != prevBranch {
					m.applyFiltersAndRebuild()
				}
				return m, nil
			}
			if key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))) {
				prevBranch := m.branchPane.SelectedBranch()
				if m.pane == paneBranches {
					m.pane = paneSessions
					m.branchPane.SetFocused(false)
				} else {
					m.pane = paneBranches
					m.branchPane.SetFocused(true)
				}
				if m.branchPane.SelectedBranch() != prevBranch {
					m.applyFiltersAndRebuild()
				}
				return m, nil
			}
		}

		// Route to the focused pane.
		if m.pane == paneBranches {
			return m.handleBranchPaneKey(msg)
		}
		return m.handleInboxKey(msg)
	}

	return m, nil
}

func (m *InboxModel) updateSessionView(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case backToInboxMsg:
		// Persist any unsent text as a draft before leaving the session.
		if m.activeConnID != "" && m.sessionView != nil {
			draft := strings.TrimSpace(m.sessionView.DraftText())
			go m.client.SetDraft(context.Background(), m.activeConnID, draft)
		}
		// Mark the session as read on close to capture any activity
		// that occurred while the user was viewing it.
		if m.activeConnID != "" {
			go m.client.MarkSessionRead(context.Background(), m.activeConnID)
		}
		m.screen = screenInbox
		m.sessionView = nil
		m.activeConnID = ""
		// Refresh data, restart spinner, and ensure the auto-refresh
		// timer is running (safety net in case it was lost).
		return m, tea.Batch(m.loadDataCmd(), m.branchPane.loadBranches(), m.autoRefreshCmd(), m.spinner.Tick)

	case openForkedSessionMsg:
		forkMsg := msg.(openForkedSessionMsg)
		// Clean up the current session view before switching.
		if m.sessionView != nil && m.sessionView.cancelEvents != nil {
			m.sessionView.cancelEvents()
		}
		if m.activeConnID != "" {
			go m.client.MarkSessionRead(context.Background(), m.activeConnID)
		}
		// Navigate to the forked session.
		return m, m.openSession(forkMsg.sessionID)

	case tea.WindowSizeMsg:
		// Forward to both.
		wMsg := msg.(tea.WindowSizeMsg)
		m.width = wMsg.Width
		m.height = wMsg.Height
		m.searchInput.SetWidth(m.sessionPaneWidth())
		m.branchPane.SetSize(m.branchPaneRenderWidth(), m.height)
		model, cmd := m.sessionView.Update(msg)
		m.sessionView = model.(*SessionViewModel)
		return m, cmd

	default:
		model, cmd := m.sessionView.Update(msg)
		m.sessionView = model.(*SessionViewModel)
		return m, cmd
	}
}

func (m *InboxModel) updateMenu(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case actionMenuCancelMsg:
		m.showMenu = false
		return m, nil

	case actionMenuResultMsg:
		m.showMenu = false
		return m, m.handleMenuAction(msg.action)

	default:
		var cmd tea.Cmd
		m.menu, cmd = m.menu.Update(msg)
		return m, cmd
	}
}

func (m *InboxModel) updateConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case confirmResultMsg:
		m.showConfirm = false
		if msg.confirmed {
			return m, m.handleConfirmAction(msg.action)
		}
		return m, nil

	default:
		var cmd tea.Cmd
		m.confirm, cmd = m.confirm.Update(msg)
		return m, cmd
	}
}

func (m *InboxModel) updateMerge(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case mergeResultMsg:
		m.showMerge = false
		if msg.err != nil {
			m.err = msg.err
		} else if msg.merged {
			// Refresh data to reflect the merge (sessions marked done,
			// worktree removed, branch deleted).
			return m, tea.Batch(m.loadDataCmd(), m.branchPane.loadBranches())
		}
		return m, nil

	default:
		cmd := m.mergeOverlay.Update(msg)
		return m, cmd
	}
}

// openComposingSession opens a composing SessionViewModel where the user
// types their first prompt. The session is created on send.
// If a branch is selected in the branch pane, it's passed through to the
// session so the backend runs in the corresponding worktree.
func (m *InboxModel) openComposingSession() tea.Cmd {
	m.screen = screenSession
	m.activeConnID = ""

	projectDir, _ := os.Getwd()
	m.sessionView = NewSessionViewComposing(m.client, projectDir)
	m.sessionView.worktreeBranch = m.branchPane.SelectedBranch()
	m.sessionView.voice = &m.voice
	m.sessionView.width = m.width
	m.sessionView.height = m.height
	if m.width > 0 {
		m.sessionView.input.SetWidth(m.width - promptInputBorderSize)
	}
	return m.sessionView.Init()
}

// --- Search mode ---

// enterSearch switches the inbox into search mode. The search bar appears
// at the top and the current session list remains visible as a starting point.
func (m *InboxModel) enterSearch() tea.Cmd {
	m.searching = true
	m.searchQuery = ""
	m.searchInput.SetValue("")
	return m.searchInput.Focus()
}

// exitSearch hides the search bar and restores the full session list.
func (m *InboxModel) exitSearch() tea.Cmd {
	m.searching = false
	m.searchQuery = ""
	m.searchInput.SetValue("")
	m.searchInput.Blur()

	// Rebuild the normal view from cached sessions (with active filters).
	if m.cachedSessions != nil {
		m.buildGroups(m.filteredSessions())
	}

	// Trigger a fresh data load to pick up any changes that occurred while searching.
	return m.loadDataCmd()
}

// filteredSessions returns cachedSessions with active structured filters
// (e.g. project filter) applied. Text search is handled separately by the
// daemon, so it is not applied here.
func (m *InboxModel) filteredSessions() []agent.SessionInfo {
	sessions := m.cachedSessions

	// Filter by project directory.
	if m.projectFilter && m.projectDir != "" {
		filtered := make([]agent.SessionInfo, 0, len(sessions))
		for _, s := range sessions {
			if s.ProjectDir == m.projectDir {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	// Filter by selected worktree. Matches sessions whose ProjectDir is the
	// selected worktree's path — works for both Clank-created worktrees and
	// the main working tree.
	wtDir := m.branchPane.SelectedWorktreeDir()
	if wtDir != "" {
		filtered := make([]agent.SessionInfo, 0, len(sessions))
		for _, s := range sessions {
			if s.ProjectDir == wtDir {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	return sessions
}

// applyFiltersAndRebuild rebuilds the inbox view from cached sessions with
// current filters applied. Call this after toggling a filter.
func (m *InboxModel) applyFiltersAndRebuild() {
	if m.cachedSessions != nil {
		m.buildGroups(m.filteredSessions())
	}
}

// updateSearch handles messages while in search mode.
func (m *InboxModel) updateSearch(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.searchInput.SetWidth(m.sessionPaneWidth())
		m.branchPane.SetSize(m.branchPaneRenderWidth(), m.height)
		return m, nil

	case inboxSearchResultMsg:
		// Only apply results if the query still matches what the user typed.
		if msg.query != m.searchQuery {
			return m, nil
		}
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.buildSearchResults(msg.sessions)
		return m, nil

	case inboxDataMsg:
		// Always cache the full session list so we can restore on exit
		// or when the search query is cleared.
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.err = nil
			m.cachedSessions = msg.sessions
			// Only rebuild from this data if not actively filtering.
			if m.searchQuery == "" {
				m.buildGroups(m.filteredSessions())
			}
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleSearchKey(msg)
	}

	return m, nil
}

// handleSearchKey processes keyboard input during search mode.
func (m *InboxModel) handleSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		return m, m.exitSearch()

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		return m, tea.Quit

	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		if m.cursor >= 0 && m.cursor < len(m.flatRows) {
			row := m.flatRows[m.cursor]
			if row.session != nil {
				return m, m.openSession(row.session.ID)
			}
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
		if m.cursor < len(m.flatRows)-1 {
			m.cursor++
		}
		return m, nil

	default:
		// Forward to text input.
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)

		// If the text changed, fire a new search.
		newValue := m.searchInput.Value()
		if newValue != m.searchQuery {
			m.searchQuery = newValue
			if newValue == "" {
				// Query cleared — restore the full session list.
				if m.cachedSessions != nil {
					m.buildGroups(m.filteredSessions())
				}
				return m, cmd
			}
			return m, tea.Batch(cmd, m.searchCmd(newValue))
		}
		return m, cmd
	}
}

// buildSearchResults populates the inbox groups/rows from search results.
// Results are shown in a single flat "Results" group, ranked by relevance
// (the order returned by the daemon).
func (m *InboxModel) buildSearchResults(sessions []agent.SessionInfo) {
	// Apply client-side structured filters (e.g. project) on top of
	// the daemon's text search results.
	if m.projectFilter && m.projectDir != "" {
		filtered := make([]agent.SessionInfo, 0, len(sessions))
		for _, s := range sessions {
			if s.ProjectDir == m.projectDir {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	if len(sessions) == 0 {
		m.groups = nil
		m.flatRows = nil
		m.cursor = 0
		return
	}

	headerStyle := lipgloss.NewStyle().Foreground(dimColor).Bold(true)
	rows := make([]inboxRow, len(sessions))
	for i := range sessions {
		rows[i] = inboxRow{session: &sessions[i]}
	}

	m.groups = []inboxGroup{{
		name:  fmt.Sprintf("Results (%d)", len(sessions)),
		style: headerStyle,
		rows:  rows,
	}}
	m.flatRows = rows

	// Clamp cursor.
	if m.cursor >= len(m.flatRows) {
		m.cursor = len(m.flatRows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *InboxModel) handleInboxKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	msg = normalizeKeyCase(msg)

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		m.cleanupVoice()
		return m, tea.Quit
	case key.Matches(msg, key.NewBinding(key.WithKeys("q"))):
		m.cleanupVoice()
		return m, tea.Quit
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
		if m.cursor > 0 {
			m.cursor--
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
		if m.cursor < len(m.flatRows)-1 {
			m.cursor++
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("pgup", "ctrl+u"))):
		m.cursor -= m.viewportHeight() / 2
		if m.cursor < 0 {
			m.cursor = 0
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown", "ctrl+d"))):
		m.cursor += m.viewportHeight() / 2
		if m.cursor >= len(m.flatRows) {
			m.cursor = len(m.flatRows) - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+up"))):
		// Jump to the previous breakpoint. Breakpoints within each group are:
		//   1. groupFirstRow (top of group)
		//   2. groupLastNonDoneRow (idle/done boundary, if distinct from first)
		if len(m.flatRows) > 0 {
			bp := m.buildBreakpoints()
			m.cursor = prevBreakpoint(bp, m.cursor)
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+down"))):
		// Jump to the next breakpoint. Breakpoints within each group are:
		//   1. groupFirstRow (top of group)
		//   2. groupLastNonDoneRow (idle/done boundary, if distinct from first)
		if len(m.flatRows) > 0 {
			bp := m.buildBreakpoints()
			m.cursor = nextBreakpoint(bp, m.cursor)
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("home", "g"))):
		m.cursor = 0
	case key.Matches(msg, key.NewBinding(key.WithKeys("end", "G"))):
		if len(m.flatRows) > 0 {
			m.cursor = len(m.flatRows) - 1
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("r"))):
		return m, m.loadDataCmd()
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		if m.cursor >= 0 && m.cursor < len(m.flatRows) {
			row := m.flatRows[m.cursor]
			if row.doneAccordion != "" {
				if m.doneExpanded == nil {
					m.doneExpanded = make(map[string]bool)
				}
				m.doneExpanded[row.doneAccordion] = !m.doneExpanded[row.doneAccordion]
				m.rebuildFlatRows()
				return m, nil
			}
			if row.accordion != "" {
				if m.archiveExpanded == nil {
					m.archiveExpanded = make(map[string]bool)
				}
				m.archiveExpanded[row.accordion] = !m.archiveExpanded[row.accordion]
				m.rebuildFlatRows()
				return m, nil
			}
			if row.session != nil {
				return m, m.openSession(row.session.ID)
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("n"))):
		return m, m.openComposingSession()
	case key.Matches(msg, key.NewBinding(key.WithKeys("/", "ctrl+f", "ctrl+k"))):
		return m, m.enterSearch()
	case key.Matches(msg, key.NewBinding(key.WithKeys("."))):
		m.projectFilter = !m.projectFilter
		m.applyFiltersAndRebuild()
	case key.Matches(msg, key.NewBinding(key.WithKeys("d"))):
		if m.cursor >= 0 && m.cursor < len(m.flatRows) {
			row := m.flatRows[m.cursor]
			if row.session != nil {
				title := row.session.Title
				if title == "" {
					title = truncateStr(row.session.Prompt, 40)
				}
				m.showConfirm = true
				m.confirm = newConfirmDialog(
					"Mark as Done",
					fmt.Sprintf("Mark '%s' as done?\nIt will be hidden from the inbox.", title),
					"done:"+row.session.ID,
				)
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("f"))):
		if m.cursor >= 0 && m.cursor < len(m.flatRows) {
			row := m.flatRows[m.cursor]
			if row.session != nil {
				return m, m.toggleFollowUp(row.session.ID)
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("x"))):
		if m.cursor >= 0 && m.cursor < len(m.flatRows) {
			row := m.flatRows[m.cursor]
			if row.session != nil {
				title := row.session.Title
				if title == "" {
					title = truncateStr(row.session.Prompt, 40)
				}
				m.showConfirm = true
				if row.session.Visibility == agent.VisibilityArchived {
					m.confirm = newConfirmDialog(
						"Unarchive Session",
						fmt.Sprintf("Unarchive '%s'?\nIt will reappear in the inbox.", title),
						"unarchive:"+row.session.ID,
					)
				} else {
					m.confirm = newConfirmDialog(
						"Archive Session",
						fmt.Sprintf("Archive '%s'?\nIt will be hidden from the inbox.", title),
						"archive:"+row.session.ID,
					)
				}
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("o"))):
		if m.cursor >= 0 && m.cursor < len(m.flatRows) {
			row := m.flatRows[m.cursor]
			if row.session != nil {
				return m, openNativeCLI(row.session)
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("w"))):
		m.branchPaneHidden = !m.branchPaneHidden
		if m.branchPaneHidden {
			m.pane = paneSessions
			m.branchPane.SetFocused(false)
		} else {
			m.pane = paneBranches
			m.branchPane.SetFocused(true)
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("?"))):
		m.showHelp = true
		return m, nil
	}
	return m, nil
}

func (m *InboxModel) openSession(sessionID string) tea.Cmd {
	m.screen = screenSession
	m.activeConnID = sessionID

	// Mark session as read so the inbox reflects the change immediately.
	go m.client.MarkSessionRead(context.Background(), sessionID)

	// Pre-subscribe to SSE before creating the view model to avoid missing
	// events from an already-busy session. The connect is to a local Unix
	// socket so it completes near-instantly.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	events, err := m.client.SubscribeEvents(sseCtx)

	m.sessionView = NewSessionViewModel(m.client, sessionID)
	m.sessionView.voice = &m.voice
	if err == nil {
		m.sessionView.SetEventChannel(events, sseCancel)
	} else {
		// Fall back to subscribing in Init() if pre-subscribe fails.
		sseCancel()
	}

	// Forward current dimensions so the session view doesn't stay at "Loading...".
	m.sessionView.width = m.width
	m.sessionView.height = m.height
	if m.width > 0 {
		m.sessionView.input.SetWidth(m.width - promptInputBorderSize)
	}

	// Restore draft text if the session has one, so the user can continue typing.
	for _, row := range m.flatRows {
		if row.session != nil && row.session.ID == sessionID && row.session.Draft != "" {
			m.sessionView.RestoreDraft(row.session.Draft)
			break
		}
	}

	return m.sessionView.Init()
}

func (m *InboxModel) handleMenuAction(action string) tea.Cmd {
	parts := strings.SplitN(action, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	verb, id := parts[0], parts[1]

	switch verb {
	case "open":
		return m.openSession(id)
	case "delete":
		return m.deleteSession(id)
	}
	return nil
}

func (m *InboxModel) deleteSession(sessionID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.client.DeleteSession(ctx, sessionID); err != nil {
			return inboxDataMsg{err: err}
		}
		// Reload data after delete.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		sessions, err := m.client.ListSessions(ctx2)
		return inboxDataMsg{sessions: sessions, err: err}
	}
}

func (m *InboxModel) handleConfirmAction(action string) tea.Cmd {
	parts := strings.SplitN(action, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	verb, id := parts[0], parts[1]

	switch verb {
	case "done":
		return m.setSessionVisibility(id, agent.VisibilityDone)
	case "archive":
		return m.setSessionVisibility(id, agent.VisibilityArchived)
	case "unarchive":
		return m.setSessionVisibility(id, agent.VisibilityVisible)
	}
	return nil
}

func (m *InboxModel) setSessionVisibility(sessionID string, visibility agent.SessionVisibility) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.client.SetVisibility(ctx, sessionID, visibility); err != nil {
			return inboxDataMsg{err: err}
		}
		// Reload data after visibility change.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		sessions, err := m.client.ListSessions(ctx2)
		return inboxDataMsg{sessions: sessions, err: err}
	}
}

func (m *InboxModel) toggleFollowUp(sessionID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := m.client.ToggleFollowUp(ctx, sessionID); err != nil {
			return inboxDataMsg{err: err}
		}
		// Reload data so the session moves to/from the FOLLOW UP group.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		sessions, err := m.client.ListSessions(ctx2)
		return inboxDataMsg{sessions: sessions, err: err}
	}
}

// sessionSortPriority returns a numeric priority for sorting within a day group.
// Busy/starting sessions float to the top; done/archived are in separate
// accordion sections so they don't normally reach this function.
// Everything else (idle, error, unread, dead, …) sorts equally by UpdatedAt.
func sessionSortPriority(s *agent.SessionInfo) int {
	switch {
	// Done/archived always sink to the bottom regardless of status.
	case s.Visibility == agent.VisibilityDone:
		return 6
	case s.Visibility == agent.VisibilityArchived:
		return 7
	case s.Status == agent.StatusBusy || s.Status == agent.StatusStarting:
		return 0
	default:
		return 5 // idle, error, unread, dead, etc.
	}
}

// buildGroups organises sessions into date-based groups (Today, Yesterday, …).
// Within each day, sessions are sorted by status priority then by UpdatedAt
// descending so busy/starting sessions appear first.
//
// Done sessions are stored in each group's doneRows and hidden behind a
// per-group accordion toggle (tracked by m.doneExpanded[label]).
// Archived sessions are stored in each group's archivedRows and hidden
// behind a per-group accordion toggle (tracked by m.archiveExpanded[label]).
func (m *InboxModel) buildGroups(sessions []agent.SessionInfo) {
	now := time.Now()

	// Sort all sessions by UpdatedAt descending so day buckets are in
	// chronological order and the most recent day appears first.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	// Bucket sessions by day label, preserving insertion order.
	// Done and archived sessions go into separate slices per bucket.
	type dayBucket struct {
		label    string
		rows     []inboxRow
		done     []inboxRow
		archived []inboxRow
	}
	var buckets []dayBucket
	bucketIdx := make(map[string]int) // label -> index into buckets

	for i := range sessions {
		s := &sessions[i]
		label := dateLabel(s.UpdatedAt, now)

		idx, ok := bucketIdx[label]
		if !ok {
			idx = len(buckets)
			bucketIdx[label] = idx
			buckets = append(buckets, dayBucket{label: label})
		}
		row := inboxRow{session: s}
		switch s.Visibility {
		case agent.VisibilityArchived:
			buckets[idx].archived = append(buckets[idx].archived, row)
		case agent.VisibilityDone:
			buckets[idx].done = append(buckets[idx].done, row)
		default:
			buckets[idx].rows = append(buckets[idx].rows, row)
		}
	}

	// Within each day, sort active rows by status priority then UpdatedAt descending.
	for i := range buckets {
		sort.SliceStable(buckets[i].rows, func(a, b int) bool {
			pa := sessionSortPriority(buckets[i].rows[a].session)
			pb := sessionSortPriority(buckets[i].rows[b].session)
			if pa != pb {
				return pa < pb
			}
			return buckets[i].rows[a].session.UpdatedAt.After(buckets[i].rows[b].session.UpdatedAt)
		})
	}

	headerStyle := lipgloss.NewStyle().Foreground(dimColor).Bold(true)
	m.groups = nil
	for _, b := range buckets {
		m.groups = append(m.groups, inboxGroup{
			name:         b.label,
			style:        headerStyle,
			rows:         b.rows,
			doneRows:     b.done,
			archivedRows: b.archived,
		})
	}

	m.rebuildFlatRows()
}

// rebuildFlatRows reconstructs flatRows from m.groups, inserting per-group
// accordion toggles and optionally expanded done/archived rows.
// Called by buildGroups and when toggling an accordion.
func (m *InboxModel) rebuildFlatRows() {
	m.flatRows = nil
	for _, g := range m.groups {
		m.flatRows = append(m.flatRows, g.rows...)
		if len(g.doneRows) > 0 {
			m.flatRows = append(m.flatRows, inboxRow{doneAccordion: g.name})
			if m.doneExpanded[g.name] {
				m.flatRows = append(m.flatRows, g.doneRows...)
			}
		}
		if len(g.archivedRows) > 0 {
			m.flatRows = append(m.flatRows, inboxRow{accordion: g.name})
			if m.archiveExpanded[g.name] {
				m.flatRows = append(m.flatRows, g.archivedRows...)
			}
		}
	}

	// Clamp cursor.
	if m.cursor >= len(m.flatRows) {
		m.cursor = len(m.flatRows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// --- View ---

// renderFilterBar renders the always-visible filter/search bar.
// When searching (text input focused): pills + text input.
// When not searching: pills + dimmed placeholder.
func (m *InboxModel) renderFilterBar() string {
	var parts []string

	// Render active filter pills.
	if m.projectFilter && m.projectName != "" {
		pill := renderPill(m.projectName, secondaryColor)
		parts = append(parts, pill)
	}

	if m.searching {
		// Focused text input — the textinput widget renders its own prompt,
		// cursor, and placeholder.
		parts = append(parts, m.searchInput.View())
	} else if m.searchQuery != "" {
		// Shouldn't normally happen (searchQuery is cleared on exit),
		// but display it if present.
		prompt := lipgloss.NewStyle().Foreground(dimColor).Render("/ ")
		text := lipgloss.NewStyle().Foreground(textColor).Render(m.searchQuery)
		parts = append(parts, prompt+text)
	} else {
		// Blurred state — show the search input so the placeholder renders.
		parts = append(parts, m.searchInput.View())
	}

	return strings.Join(parts, " ")
}

// renderPill renders a styled filter pill badge, e.g. "[clank]".
func renderPill(label string, fg color.Color) string {
	return lipgloss.NewStyle().
		Foreground(fg).
		Bold(true).
		Render("[" + label + "]")
}

func (m *InboxModel) View() tea.View {
	if m.screen == screenSession && m.sessionView != nil {
		return m.sessionView.View()
	}

	if m.width == 0 {
		v := newVoiceEnabledView("Loading...")
		return v
	}

	sessionContent := m.renderSessionPane()
	var content string

	if m.showTwoPanes() {
		branchView := m.branchPane.View()
		content = lipgloss.JoinHorizontal(lipgloss.Top, branchView, " ", sessionContent)
	} else {
		content = sessionContent
	}

	// Overlay menu if open.
	if m.showMenu {
		content = m.overlayMenu(content)
	}

	// Overlay confirm dialog if open.
	if m.showConfirm {
		content = m.overlayConfirm(content)
	}

	// Overlay merge dialog if open.
	if m.showMerge {
		content = overlayCenter(content, m.mergeOverlay.View(), m.width, m.height)
	}

	// Overlay help if open.
	if m.showHelp {
		content = m.overlayHelp(content)
	}

	// Overlay Kitty keyboard warning if shown.
	if m.showKittyWarning {
		content = m.overlayKittyWarning(content)
	}

	v := newVoiceEnabledView(content)
	return v
}

// renderSessionPane renders the right pane (session list) content as a string.
func (m *InboxModel) renderSessionPane() string {
	var sb strings.Builder

	// Header.
	headerText := "CLANK  Inbox"
	header := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render(headerText)
	badge := voiceHeaderBadge(m.voice)
	if badge != "" {
		header = header + " " + badge
	}
	sb.WriteString(header)
	sb.WriteString("\n\n")

	// Filter bar — always visible below the header.
	sb.WriteString(m.renderFilterBar())
	sb.WriteString("\n\n")

	// Error.
	if m.err != nil {
		errMsg := lipgloss.NewStyle().Foreground(dangerColor).Render(fmt.Sprintf("Error: %v", m.err))
		sb.WriteString(errMsg)
		sb.WriteString("\n\n")
	}

	// Build display lines.
	m.buildDisplayLines()
	// Highlight selected row (only when session pane has focus).
	if m.pane == paneSessions && m.cursor >= 0 && m.cursor < len(m.flatRows) {
		lineIdx := m.rowToLine[m.cursor]
		if lineIdx < len(m.displayLines) {
			row := m.flatRows[m.cursor]
			if row.doneAccordion != "" {
				m.displayLines[lineIdx] = m.renderDoneAccordion(row.doneAccordion, true)
			} else if row.accordion != "" {
				m.displayLines[lineIdx] = m.renderArchiveAccordion(row.accordion, true)
			} else {
				m.displayLines[lineIdx] = m.renderRow(row, true)
			}
		}
	}
	m.ensureCursorVisible()

	// Render visible window.
	vh := m.viewportHeight()
	end := m.scrollOffset + vh
	if end > len(m.displayLines) {
		end = len(m.displayLines)
	}
	for i := m.scrollOffset; i < end; i++ {
		sb.WriteString(m.displayLines[i])
		sb.WriteString("\n")
	}
	// Pad remaining lines.
	for i := end - m.scrollOffset; i < vh; i++ {
		sb.WriteString("\n")
	}

	// Scroll indicator.
	totalLines := len(m.displayLines)
	if totalLines > vh {
		pct := 0
		if totalLines-vh > 0 {
			pct = m.scrollOffset * 100 / (totalLines - vh)
		}
		scrollInfo := lipgloss.NewStyle().Foreground(dimColor).Render(
			fmt.Sprintf("  [%d/%d lines, %d%%]", m.scrollOffset+1, totalLines, pct))
		sb.WriteString(scrollInfo)
	}

	// Help bar.
	var help string
	if m.searching {
		help = helpStyle.Render("esc: cancel | enter: open | .: this project | up/down: navigate")
	} else {
		parts := []string{voiceHelpItem(m.voice), "enter: open", "n: new", "/: search", "w: worktrees", "?: help", "q: quit"}
		help = helpStyle.Render(strings.Join(parts, " | "))
	}
	sb.WriteString(help)

	return sb.String()
}

// --- Two-pane layout helpers ---

// minTwoPaneWidth is the minimum terminal width to show both panes.
// Below this, only the session pane is shown.
const minTwoPaneWidth = 80

// showTwoPanes returns true when the terminal is wide enough and the user
// hasn't manually hidden the sidebar with 'w'.
func (m *InboxModel) showTwoPanes() bool {
	return m.width >= minTwoPaneWidth && !m.branchPaneHidden
}

// branchPaneRenderWidth returns the width allocated to the branch pane (including border).
func (m *InboxModel) branchPaneRenderWidth() int {
	return branchPaneWidth
}

// sessionPaneWidth returns the width available for the session list pane.
func (m *InboxModel) sessionPaneWidth() int {
	if m.showTwoPanes() {
		return m.width - m.branchPaneRenderWidth() - 1 // 1 for gap
	}
	return m.width
}

// updateBranchSessionCounts computes per-branch active session counts
// from cachedSessions and passes them to the branch pane for display.
// Sessions are attributed to branches by matching ProjectDir against
// worktree paths, not by the WorktreeBranch field.
func (m *InboxModel) updateBranchSessionCounts() {
	wtDirToBranch := m.branchPane.WorktreeDirToBranch()
	counts := make(map[string]int)
	for _, s := range m.cachedSessions {
		if s.Visibility == agent.VisibilityArchived {
			continue
		}
		if branch, ok := wtDirToBranch[s.ProjectDir]; ok {
			counts[branch]++
		}
	}
	m.branchPane.SetSessionCounts(counts)
}

// handleBranchPaneKey forwards key events to the branch pane while it's focused.
// Global keys (q, ctrl+c) are still handled at the inbox level.
func (m *InboxModel) handleBranchPaneKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	msg = normalizeKeyCase(msg)

	// Global keys handled even when branch pane is focused.
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		m.cleanupVoice()
		return m, tea.Quit
	case key.Matches(msg, key.NewBinding(key.WithKeys("q"))):
		// Don't quit while typing a branch name.
		if m.branchPane.creating {
			break
		}
		m.cleanupVoice()
		return m, tea.Quit
	case key.Matches(msg, key.NewBinding(key.WithKeys("w"))):
		// Don't toggle sidebar while typing a branch name.
		if m.branchPane.creating {
			break
		}
		m.branchPaneHidden = !m.branchPaneHidden
		if m.branchPaneHidden {
			m.pane = paneSessions
			m.branchPane.SetFocused(false)
		} else {
			m.pane = paneBranches
			m.branchPane.SetFocused(true)
		}
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("?"))):
		m.showHelp = true
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("m"))):
		// Don't open merge overlay while typing a branch name.
		if m.branchPane.creating {
			break
		}
		bi := m.branchPane.SelectedBranchInfo()
		if bi != nil && !bi.IsDefault {
			m.mergeOverlay = newMergeOverlay(m.client, m.branchPane.projectDir, *bi)
			m.mergeOverlay.SetSize(m.width, m.height)
			m.showMerge = true
		}
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		// While creating a new branch, let the branch pane handle Enter.
		if m.branchPane.creating {
			break
		}
		// Enter on a branch selects it and switches focus to session pane.
		prevBranch := m.branchPane.SelectedBranch()
		m.pane = paneSessions
		m.branchPane.SetFocused(false)
		if m.branchPane.SelectedBranch() != prevBranch {
			m.applyFiltersAndRebuild()
		}
		return m, nil
	}

	// Track branch selection before and after to detect changes.
	prevBranch := m.branchPane.SelectedBranch()
	cmd := m.branchPane.Update(msg)
	if m.branchPane.SelectedBranch() != prevBranch {
		m.applyFiltersAndRebuild()
	}
	return m, cmd
}

func (m *InboxModel) buildDisplayLines() {
	m.displayLines = nil
	m.rowToLine = make([]int, len(m.flatRows))

	flatIdx := 0
	for gi, g := range m.groups {
		// Group header.
		m.displayLines = append(m.displayLines, g.style.Render(g.name))

		// Active session rows.
		for ri := range g.rows {
			m.rowToLine[flatIdx] = len(m.displayLines)
			m.displayLines = append(m.displayLines, m.renderRow(g.rows[ri], false))
			flatIdx++
		}

		// Per-group done accordion + expanded rows.
		if len(g.doneRows) > 0 {
			// Accordion toggle row.
			m.rowToLine[flatIdx] = len(m.displayLines)
			m.displayLines = append(m.displayLines, m.renderDoneAccordion(g.name, false))
			flatIdx++

			// Expanded done session rows.
			if m.doneExpanded[g.name] {
				for range g.doneRows {
					m.rowToLine[flatIdx] = len(m.displayLines)
					m.displayLines = append(m.displayLines, m.renderRow(m.flatRows[flatIdx], false))
					flatIdx++
				}
			}
		}

		// Per-group archive accordion + expanded rows.
		if len(g.archivedRows) > 0 {
			// Accordion toggle row.
			m.rowToLine[flatIdx] = len(m.displayLines)
			m.displayLines = append(m.displayLines, m.renderArchiveAccordion(g.name, false))
			flatIdx++

			// Expanded archived session rows.
			if m.archiveExpanded[g.name] {
				for range g.archivedRows {
					m.rowToLine[flatIdx] = len(m.displayLines)
					m.displayLines = append(m.displayLines, m.renderRow(m.flatRows[flatIdx], false))
					flatIdx++
				}
			}
		}

		// Blank separator between groups (not after the last one).
		if gi < len(m.groups)-1 {
			m.displayLines = append(m.displayLines, "")
		}
	}

	if len(m.flatRows) == 0 {
		var emptyMsg string
		if m.searching && m.searchQuery != "" {
			emptyMsg = "No matching sessions."
		} else {
			emptyMsg = "No sessions. Press 'n' to start a new session, or run 'clank code <prompt>'."
		}
		m.displayLines = append(m.displayLines,
			lipgloss.NewStyle().Foreground(mutedColor).Render(emptyMsg))
	}
}

// renderDoneAccordion renders the collapsible done toggle line for a date group.
func (m *InboxModel) renderDoneAccordion(groupLabel string, selected bool) string {
	chevron := "▸"
	if m.doneExpanded[groupLabel] {
		chevron = "▾"
	}
	// Find the done count for this group.
	count := 0
	for _, g := range m.groups {
		if g.name == groupLabel {
			count = len(g.doneRows)
			break
		}
	}
	label := fmt.Sprintf("%s ✓ Done (%d)", chevron, count)

	style := lipgloss.NewStyle().Foreground(successColor)
	if selected {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		return prefix + style.Render(label)
	}
	return "  " + style.Render(label)
}

// renderArchiveAccordion renders the collapsible archive toggle line for a date group.
func (m *InboxModel) renderArchiveAccordion(groupLabel string, selected bool) string {
	chevron := "▸"
	if m.archiveExpanded[groupLabel] {
		chevron = "▾"
	}
	// Find the archived count for this group.
	count := 0
	for _, g := range m.groups {
		if g.name == groupLabel {
			count = len(g.archivedRows)
			break
		}
	}
	label := fmt.Sprintf("%s Archive (%d)", chevron, count)

	style := lipgloss.NewStyle().Foreground(mutedColor)
	if selected {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		return prefix + style.Render(label)
	}
	return "  " + style.Render(label)
}

func (m *InboxModel) renderRow(row inboxRow, selected bool) string {
	if row.session == nil {
		return ""
	}

	s := row.session
	isDone := s.Visibility == agent.VisibilityDone
	isArchived := s.Visibility == agent.VisibilityArchived
	ago := timeAgo(s.UpdatedAt)
	stateIcon := m.styledAgentStatus(s.Status)

	unreadMark := " "
	if s.FollowUp {
		unreadMark = lipgloss.NewStyle().Foreground(warningColor).Bold(true).Render("!")
	} else if s.Unread() {
		unreadMark = lipgloss.NewStyle().Foreground(dangerColor).Bold(true).Render("*")
	}

	// Agent mode badge — colored so users can quickly triage build vs plan sessions.
	agentBadge := fmt.Sprintf("%-5s", "")
	if s.Agent != "" {
		badgeColor := agentColor(s.Agent)
		if isDone || isArchived {
			badgeColor = mutedColor
		}
		agentBadge = lipgloss.NewStyle().Foreground(badgeColor).Render(fmt.Sprintf("%-5s", s.Agent))
	}

	// Branch badge — shown only when viewing all branches (no branch filter active).
	const branchBadgeWidth = 12
	branchBadge := ""
	branchExtra := 0
	if m.branchPane.SelectedBranch() == "" && s.WorktreeBranch != "" {
		branchLabel := s.WorktreeBranch
		if len(branchLabel) > branchBadgeWidth-2 {
			branchLabel = branchLabel[:branchBadgeWidth-3] + "…"
		}
		branchLabel = fmt.Sprintf("%-*s", branchBadgeWidth-2, branchLabel)
		bc := secondaryColor
		if isDone || isArchived {
			bc = mutedColor
		}
		branchBadge = lipgloss.NewStyle().Foreground(bc).Render(branchLabel) + " "
		branchExtra = branchBadgeWidth - 1 // visible width including trailing space
	}

	paddedProject := fmt.Sprintf("%-12s", s.ProjectName)
	projectColor := secondaryColor
	if isDone {
		projectColor = mutedColor
	} else if isArchived {
		projectColor = mutedColor
	}
	styledProject := lipgloss.NewStyle().Foreground(projectColor).Render(paddedProject)

	// Fixed-width columns before the prompt: "  " (2) + project (12) + " " (1) + stateIcon (1) + " " (1) + agent (5) + " " (1) + unread (1) + " " (1)
	// We also reserve 9 chars on the right for the timestamp (8 chars padded + 1 space).
	const agoWidth = 9
	const draftSuffix = " draft"                         // 6 chars when present
	leftFixedWidth := 2 + 12 + 1 + 1 + 1 + 5 + 1 + 1 + 1 // 25
	draftExtra := 0
	if s.Draft != "" {
		draftExtra = len(draftSuffix)
	}
	maxPromptWidth := m.sessionPaneWidth() - leftFixedWidth - branchExtra - agoWidth - draftExtra
	if maxPromptWidth < 10 {
		maxPromptWidth = 10
	}

	prompt := truncateStr(s.Prompt, maxPromptWidth)
	if s.Title != "" {
		prompt = truncateStr(s.Title, maxPromptWidth)
	}
	if prompt == "" {
		prompt = lipgloss.NewStyle().Foreground(dimColor).Render(truncateStr(s.ID, 8))
	} else if isArchived {
		// Archived sessions: fully grayed-out title.
		prompt = lipgloss.NewStyle().Foreground(mutedColor).Render(prompt)
	} else if isDone {
		// Done sessions: green title text.
		prompt = lipgloss.NewStyle().Foreground(successColor).Render(prompt)
	} else if s.FollowUp {
		// Follow-up sessions: dark orange title to stand out.
		prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("#D97706")).Bold(true).Render(prompt)
	} else if s.Unread() {
		// Unread sessions: bold title to stand out.
		prompt = lipgloss.NewStyle().Bold(true).Render(prompt)
	} else {
		// Read sessions: dimmed title.
		prompt = lipgloss.NewStyle().Foreground(dimColor).Render(prompt)
	}

	// Append lowercase red "draft" label right after the title.
	if s.Draft != "" {
		prompt += lipgloss.NewStyle().Foreground(dangerColor).Render(draftSuffix)
	}

	styledAgo := lipgloss.NewStyle().Foreground(dimColor).Render(ago)

	// Build the left portion of the line (everything except the timestamp).
	left := fmt.Sprintf("  %s %s %s %s%s %s",
		styledProject,
		stateIcon,
		agentBadge,
		branchBadge,
		unreadMark,
		prompt,
	)

	// Pad the gap between left content and right-aligned timestamp.
	// Use ANSI-unaware length for the visible width of left.
	leftVisible := lipgloss.Width(left)
	agoVisible := lipgloss.Width(styledAgo)
	gap := m.sessionPaneWidth() - leftVisible - agoVisible
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + styledAgo

	if selected {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		return prefix + line[2:]
	}
	return line
}

func (m *InboxModel) styledAgentStatus(status agent.SessionStatus) string {
	switch status {
	case agent.StatusBusy, agent.StatusStarting:
		return m.spinner.View()
	case agent.StatusIdle:
		return lipgloss.NewStyle().Foreground(warningColor).Render("○")
	case agent.StatusError:
		return lipgloss.NewStyle().Foreground(dangerColor).Render("✗")
	case agent.StatusDead:
		return lipgloss.NewStyle().Foreground(mutedColor).Render("·")
	default:
		return lipgloss.NewStyle().Foreground(dimColor).Render("·")
	}
}

func (m *InboxModel) viewportHeight() int {
	reserved := 6 // header (1) + blank (1) + filter bar (1) + blank (1) + help bar (~1) + padding (1)
	if m.err != nil {
		reserved += 2
	}
	h := m.height - reserved
	if h < 3 {
		h = 3
	}
	return h
}

// groupFlatRowCount returns the number of flatRows occupied by the given group,
// including active rows, accordion toggles (if any), and expanded done/archived rows.
func (m *InboxModel) groupFlatRowCount(g inboxGroup) int {
	n := len(g.rows)
	if len(g.doneRows) > 0 {
		n++ // done accordion toggle
		if m.doneExpanded[g.name] {
			n += len(g.doneRows)
		}
	}
	if len(g.archivedRows) > 0 {
		n++ // archive accordion toggle
		if m.archiveExpanded[g.name] {
			n += len(g.archivedRows)
		}
	}
	return n
}

// cursorGroupIndex returns the index into m.groups for the group that
// contains the current cursor position. Returns 0 if flatRows is empty.
func (m *InboxModel) cursorGroupIndex() int {
	offset := 0
	for i, g := range m.groups {
		count := m.groupFlatRowCount(g)
		if m.cursor < offset+count {
			return i
		}
		offset += count
	}
	return len(m.groups) - 1
}

// groupFirstRow returns the flatRows index of the first row in the given group.
func (m *InboxModel) groupFirstRow(groupIdx int) int {
	offset := 0
	for i := 0; i < groupIdx; i++ {
		offset += m.groupFlatRowCount(m.groups[i])
	}
	return offset
}

// groupLastActiveRow returns the flatRows index of the last active (non-accordion)
// session row in the given group. Returns -1 if the group has no active session rows.
func (m *InboxModel) groupLastActiveRow(groupIdx int) int {
	g := m.groups[groupIdx]
	if len(g.rows) == 0 {
		return -1
	}
	offset := m.groupFirstRow(groupIdx)
	return offset + len(g.rows) - 1
}

// buildBreakpoints returns the sorted, deduplicated list of flatRow indices
// that shift+up/down should cycle through. For each date group the breakpoints
// are the first row and (if distinct) the last active session row.
func (m *InboxModel) buildBreakpoints() []int {
	var bp []int
	for gi := range m.groups {
		first := m.groupFirstRow(gi)
		bp = append(bp, first)
		if boundary := m.groupLastActiveRow(gi); boundary >= 0 && boundary != first {
			bp = append(bp, boundary)
		}
	}
	return bp
}

// nextBreakpoint returns the smallest breakpoint strictly greater than cursor.
// If cursor is already at or past the last breakpoint, it returns the last one.
func nextBreakpoint(bp []int, cursor int) int {
	for _, b := range bp {
		if b > cursor {
			return b
		}
	}
	return bp[len(bp)-1]
}

// prevBreakpoint returns the largest breakpoint strictly less than cursor.
// If cursor is already at or before the first breakpoint, it returns the first one.
func prevBreakpoint(bp []int, cursor int) int {
	for i := len(bp) - 1; i >= 0; i-- {
		if bp[i] < cursor {
			return bp[i]
		}
	}
	return bp[0]
}

func (m *InboxModel) ensureCursorVisible() {
	if len(m.flatRows) == 0 {
		m.scrollOffset = 0
		return
	}
	vh := m.viewportHeight()
	cursorLine := m.rowToLine[m.cursor]

	// Keep the cursor away from the edges of the viewport by maintaining
	// a scroll margin of ~10% of the viewport height (minimum 2 lines).
	margin := vh * 10 / 100
	if margin < 2 {
		margin = 2
	}
	// Margin can't exceed half the viewport, otherwise the two margins
	// overlap and the cursor has nowhere valid to sit.
	if margin > vh/2 {
		margin = vh / 2
	}

	// If the cursor is too close to the top, scroll up.
	if cursorLine < m.scrollOffset+margin {
		m.scrollOffset = cursorLine - margin
	}
	// If the cursor is too close to the bottom, scroll down.
	if cursorLine >= m.scrollOffset+vh-margin {
		m.scrollOffset = cursorLine - vh + margin + 1
	}

	// Clamp to valid range.
	maxOffset := len(m.displayLines) - vh
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
}

func (m *InboxModel) overlayMenu(base string) string {
	return overlayCenter(base, m.menu.View(), m.width, m.height)
}

func (m *InboxModel) overlayConfirm(base string) string {
	return overlayCenter(base, m.confirm.View(), m.width, m.height)
}

func (m *InboxModel) overlayHelp(base string) string {
	var sb strings.Builder

	innerWidth := 44

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(textColor).
		Width(innerWidth).
		Render("Keybindings")
	sb.WriteString(title)
	sb.WriteString("\n")

	sep := lipgloss.NewStyle().
		Foreground(mutedColor).
		Render(strings.Repeat("─", innerWidth))

	helpLine := func(key, desc string) {
		k := lipgloss.NewStyle().Foreground(textColor).Bold(true).Render(key)
		d := lipgloss.NewStyle().Foreground(dimColor).Render(desc)
		padding := innerWidth - lipgloss.Width(k) - lipgloss.Width(d)
		if padding < 1 {
			padding = 1
		}
		sb.WriteString(k + strings.Repeat(" ", padding) + d + "\n")
	}

	// Navigation section.
	sb.WriteString(lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("Navigation"))
	sb.WriteString("\n")
	helpLine("j / k", "move down / up")
	helpLine("shift+down/up", "jump to next group")
	helpLine("g / G", "go to top / bottom")
	helpLine("ctrl+d / ctrl+u", "half-page down / up")
	sb.WriteString(sep + "\n")

	// Actions section.
	sb.WriteString(lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("Actions"))
	sb.WriteString("\n")
	helpLine("enter", "open session")
	helpLine("n", "new session")
	helpLine("o", "open in native CLI")
	helpLine("/", "search")
	helpLine("r", "refresh")
	sb.WriteString(sep + "\n")

	// Session management section.
	sb.WriteString(lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("Session"))
	sb.WriteString("\n")
	helpLine("f", "toggle follow-up")
	helpLine("d", "mark as done")
	helpLine("x", "archive / unarchive")
	sb.WriteString(sep + "\n")

	// Branches section.
	sb.WriteString(lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("Worktrees"))
	sb.WriteString("\n")
	helpLine("w", "toggle worktree sidebar")
	helpLine("tab", "switch panes")
	helpLine("n", "new branch (in sidebar)")
	helpLine("m", "merge branch (in sidebar)")
	helpLine("r", "refresh branches")
	sb.WriteString(sep + "\n")

	// Voice section.
	sb.WriteString(lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("Voice"))
	sb.WriteString("\n")
	helpLine("space (hold)", "push-to-talk")
	sb.WriteString(sep + "\n")

	helpLine("q", "quit")

	sb.WriteString("\n")
	hint := lipgloss.NewStyle().Foreground(dimColor).Render("press any key to dismiss")
	sb.WriteString(hint)

	popup := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())

	return overlayCenter(base, popup, m.width, m.height)
}
