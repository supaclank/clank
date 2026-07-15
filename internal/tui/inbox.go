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
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
)

// inboxScreen tracks which screen is active within the inbox app.
type inboxScreen int

const (
	screenInbox    inboxScreen = iota
	screenSession              // Viewing a specific session (or composing a new one)
	screenSettings             // Viewing the settings page in the right pane
	screenCloud                // Viewing the cloud (cloud auth + /me) panel
)

// inboxPane tracks which pane has keyboard focus in the two-pane layout.
type inboxPane int

const (
	paneSessions inboxPane = iota // Right pane: session list (default)
	paneSidebar                   // Sidebar: branch/worktree list
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
// It uses a sidebar + main layout: sidebar shows branches, main area shows
// sessions. In narrow terminals, only the session pane is shown.
type InboxModel struct {
	client *daemonclient.Client

	// Two-pane layout state.
	pane              inboxPane    // which pane has keyboard focus
	sidebar           SidebarModel // sidebar: branch list
	sidebarHidden     bool         // true when user toggled sidebar off with 'w'
	sidebarWidthRatio int          // sidebar width as % of screen width; adjusted with +/-

	// chatPaneX is the outer-frame column where the chat pane's leftmost
	// cell renders, cached each View() from the actual sidebar render
	// width. Mouse-X translation and the chat cursor shift both read it so
	// clicks land where the caret is drawn. 0 in single-pane mode.
	chatPaneX int

	// mouseDragOwner pins an active mouse drag to the pane that
	// received its MouseClickMsg, so motion/release events that the
	// user drags across the pane boundary still reach the originating
	// pane. nil between drags. Distinguishes "no drag" from the
	// zero-valued inboxPane (paneSessions == iota 0) without a parallel
	// bool flag.
	mouseDragOwner *inboxPane

	// Inbox list state (right pane).
	groups       []inboxGroup
	flatRows     []inboxRow
	cursor       int
	scrollOffset int
	showMenu     bool
	menu         actionMenuModel

	// notice is a transient success message cleared on the next action.
	notice string

	// Archive accordion state — tracks which date groups have their archive expanded.
	archiveExpanded map[string]bool // keyed by date group label

	// Done accordion state — tracks which date groups have their done section expanded.
	doneExpanded map[string]bool // keyed by date group label

	// Confirm dialog state.
	showConfirm bool
	confirm     confirmDialogModel

	// Import Sessions modal state.
	showImportSessions bool
	importSessions     importSessionsModel

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

	// inboxEventsCancel tears down the SSE subscription opened by
	// subscribeInboxEvents. Replaced on every reconnect.
	inboxEventsCancel context.CancelFunc
	inboxEventsCh     <-chan agent.Event

	// lastSessionsCacheSig fingerprints the most recently disk-cached
	// session list so the autoRefresh poll doesn't rewrite the cache
	// when the data hasn't materially changed. Seeded from the cache
	// loaded at startup so the first daemon poll that returns identical
	// data is a no-op write.
	lastSessionsCacheSig sessionsCacheSignature

	// Filter state — structured filters applied as pills in the search bar.
	projectDir    string // absolute path of the cwd when the inbox was launched
	projectName   string // basename of projectDir, used for the filter pill label
	projectFilter bool   // when true, only show sessions whose canonical GitRef matches gitRef

	// Repo identity for branch/worktree ops. Resolved from cwd at startup
	// via daemonclient.ResolveRepo. If resolution failed (e.g. cwd not in a
	// git repo with an origin remote), these stay zero and the sidebar will
	// surface the underlying load error.
	hostname string
	gitRef   agent.GitRef

	// Pre-built display data.
	displayLines []string
	rowToLine    []int

	// Session detail sub-view.
	screen       inboxScreen
	sessionView  *SessionViewModel
	activeConnID string // session ID of the detail view

	// Compose-overlay snapshot: captured when "n" opens the compose
	// view so Esc can restore the right pane to what was showing
	// before, rather than dumping the user on the inbox screen.
	// Cleared on a successful submit (the new chat takes over) or on
	// close. activeCompose is the discriminator — when false, no
	// snapshot is being held.
	activeCompose       bool
	preComposeScreen    inboxScreen
	preComposeSession   *SessionViewModel
	preComposeConnID    string
	preComposePane      inboxPane
	preComposeActiveRow string // sidebar's activeSessionID at snapshot time

	// sidebarWasFocused remembers whether the sidebar held focus at the
	// moment 'w' hid it, so toggling back restores the original focus
	// (preserves the inverse property of the toggle).
	sidebarWasFocused bool

	// Settings page state (shown when screen == screenSettings).
	settings settingsView

	// Cloud panel state (shown when screen == screenCloud). Persistent
	// across hovers — the panel does its own /me caching by simply
	// keeping m.me / m.session populated between visits, so re-hovering
	// the ☁ row doesn't re-fire the request. Refresh requires either a
	// sign-out + sign-in or pressing 'r' inside the panel.
	cloud            cloudView
	cloudInitialized bool

	// Color-scheme picker overlay (modal). Rendered on top of whatever
	// screen is currently active. Showing is independent of `screen` so
	// the user can tweak theming from anywhere in the future.
	showThemePicker bool
	themePicker     themePickerModel

	// Provider auth modal. Walks the user through a device-flow login
	// for an AI provider (Phase 1: GitHub Copilot only).
	showProviderAuth bool
	providerAuth     providerAuthModel

	// Cloud URL picker modal. Lets the user select from known cloud
	// providers or enter a custom URL.
	showCloudURLPicker bool
	cloudURLPicker     cloudURLPickerModel

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

// resolveLocalRepo turns the user's current working directory into the
// (hostname, canonical GitRef) the Hub expects on StartRequest. It is the
// shell→API bridge: shells out to git for the repo root + origin URL.
//
// Sends BOTH LocalPath (the repo root, used by co-located hosts to skip
// cloning) AND RemoteURL when available (cross-host stable identity,
// fallback if a remote host doesn't have LocalPath). See agent.GitRef
// godoc for resolution precedence on the host.
//
// On any failure (cwd not in a git repo) it returns zero values;
// callers surface the underlying problem via the subsequent sidebar
// branch-load error rather than blocking startup. Repos with no origin
// are still usable on the local host.
//
// Hostname is currently always HostLocal; remote hosts will be plumbed
// through when they exist.
func resolveLocalRepo(cwd string) (string, agent.GitRef) {
	root, err := git.RepoRoot(cwd)
	if err != nil {
		return "", agent.GitRef{}
	}
	ref := agent.GitRef{LocalPath: root}
	if id, _ := agent.ReadLocalWorktreeID(root); id != "" {
		ref.WorktreeID = id
	}
	if err := ref.Validate(); err != nil {
		return "", agent.GitRef{}
	}
	return host.HostLocal, ref
}

// NewInboxModel creates the inbox TUI connected to the given daemon client.
func NewInboxModel(client *daemonclient.Client) *InboxModel {
	// Apply the user's persisted color scheme (if any) before any styles
	// are constructed for this session. Unknown names silently fall back
	// to the default scheme so a corrupt preferences file can't brick the
	// TUI.
	prefs, _ := config.LoadPreferences()
	applySchemeFromPreference(prefs.ColorScheme)

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
	// Resolve cwd → (hostname, gitRef). On failure we leave both zero; the
	// sidebar's branch load will then return a clear error to the user.
	// The host adds the repo to its registry implicitly on the first
	// CreateSession that carries Dir (§7.5).
	hostname, gitRef := resolveLocalRepo(cwd)
	bp := NewSidebarModel(client, hostname, gitRef, cwd)
	bp.SetExpanded(prefs.SidebarExpanded)
	bp.SetCloudStatus(loadCloudAuthStatus())

	// Seed the sidebar (and inbox row groups) from the on-disk session
	// cache so the very first frame after launch shows real content
	// instead of waiting on the ~0.5–1s daemon round trip. The live
	// load fires from Init() and replaces this cached snapshot as
	// soon as it arrives.
	cachedSessions, _ := loadSessionsCache()
	if len(cachedSessions) > 0 {
		bp.SetSessions(cachedSessions)
	}

	m := &InboxModel{
		client:               client,
		pane:                 paneSessions,
		sidebar:              bp,
		spinner:              sp,
		searchInput:          ti,
		projectDir:           cwd,
		projectName:          filepath.Base(cwd),
		hostname:             hostname,
		gitRef:               gitRef,
		sidebarWidthRatio:    sidebarWidthRatioFromPrefs(prefs),
		sidebarHidden:        prefs.SidebarHidden,
		cachedSessions:       cachedSessions,
		lastSessionsCacheSig: sessionsCacheSig(cachedSessions),
	}
	if len(cachedSessions) > 0 {
		m.buildGroups(m.filteredSessions())
	}
	return m
}

func (m *InboxModel) Init() tea.Cmd {
	// Note: discoverCmd is intentionally NOT in the Init batch. Each TUI
	// startup used to fire discover, which races with in-flight CreateSession
	// calls in other TUIs/agents and produced duplicate inbox rows. Discover
	// at daemon startup (hub/discover_startup.go) covers the cold-boot case;
	// future explicit rediscover will be a keybind.
	cmds := []tea.Cmd{func() tea.Msg { return tea.RequestWindowSize }, m.loadDataCmd(), m.subscribeInboxEvents(), m.spinner.Tick, m.sidebar.Init()}
	// Eagerly initialize the cloud view so a saved token gets verified
	// against the server in the background, even if the user never opens
	// the cloud panel. This drives the sidebar indicator from "checking"
	// to "online" / "unavailable" without requiring a hover.
	if !m.cloudInitialized {
		m.cloud = newCloudView()
		m.cloudInitialized = true
		cmds = append(cmds, m.cloud.Init())
	}
	if m.screen == screenSession && m.sessionView != nil {
		cmds = append(cmds, m.sessionView.Init())
	}
	// Restore the last opened session for this cwd. Skipped if no
	// record exists or the session was deleted in the meantime — the
	// later session list refresh will surface the missing-id problem
	// gracefully (session-not-found error in the right pane) without
	// blocking startup.
	//
	// Restore explicitly focuses the chat pane: this isn't a sidebar
	// Enter (which keeps focus in the sidebar by design), it's "drop
	// the user where they left off." Otherwise the cursor lands
	// nowhere visible on first paint — arrow keys would move the
	// chat's hidden cursor but nothing would highlight.
	if id := lastSessionForCwd(m.projectDir); id != "" {
		cmds = append(cmds, m.openSession(id))
		m.setPane(paneSessions)
	}
	return tea.Batch(cmds...)
}

// lastSessionForCwd looks up the session id that was open last time
// the TUI exited from this cwd. Returns "" when no record is found,
// or when preferences fail to load (cold install / corrupt file).
func lastSessionForCwd(cwd string) string {
	if cwd == "" {
		return ""
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		return ""
	}
	return prefs.LastSessionByCwd[cwd]
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
		_ = m.client.Sessions().Discover(ctx, agent.BackendOpenCode, cwd)
		_ = m.client.Sessions().Discover(ctx, agent.BackendClaudeCode, cwd)
		// After discovery completes, trigger a refresh to show new sessions.
		return inboxRefreshMsg{}
	}
}

// discoverResultMsg carries the outcome of a user-initiated import.
type discoverResultMsg struct {
	imported int // number of sessions imported (best-effort count)
	err      error
}

// discoverForProvidersCmd runs discovery for the given providers and returns
// a discoverResultMsg with the number of newly-imported sessions.
func (m *InboxModel) discoverForProvidersCmd(providers []importProvider) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		cwd, err := os.Getwd()
		if err != nil {
			return discoverResultMsg{err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Count sessions before discovery so we can report a delta.
		before, _ := client.Sessions().List(ctx)
		beforeCount := len(before)

		for _, p := range providers {
			var backend agent.BackendType
			switch p {
			case importProviderClaude:
				backend = agent.BackendClaudeCode
			case importProviderOpenCode:
				backend = agent.BackendOpenCode
			default:
				continue
			}
			_ = client.Sessions().Discover(ctx, backend, cwd)
		}

		after, _ := client.Sessions().List(ctx)
		imported := len(after) - beforeCount
		if imported < 0 {
			imported = 0
		}
		return discoverResultMsg{imported: imported}
	}
}

// loadDataCmd fetches sessions from the daemon, plus the active
// remote's worktree-ownership map so the sidebar can render the ☁
// glyph for sprite-owned entries. Failures on the worktree fetch are
// swallowed — they aren't essential for showing sessions, and a
// local-only daemon legitimately has no remote to query.
func (m *InboxModel) loadDataCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		sessions, err := m.client.Sessions().List(ctx)
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

// autoRefreshCmd was a 3s polling tick that paged the daemon for the
// full session list. Replaced by SSE-driven push updates in
// inbox_sse.go; kept removed intentionally — re-adding polling on top
// of push updates would re-introduce the visible flicker the SSE
// migration was designed to eliminate.

// searchCmd performs a case-insensitive substring search against the daemon.
// Supports pipe-separated OR groups with space-separated AND terms.
func (m *InboxModel) searchCmd(query string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		sessions, err := m.client.Sessions().Search(ctx, agent.SearchParams{Query: query})
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
		// Feed the freshly-advanced spinner frame to the sidebar so
		// the cloud "checking" indicator animates without the sidebar
		// owning its own ticker.
		m.sidebar.SetCloudSpinnerFrame(m.spinner.View())
		m.sidebar.AdvanceTitleAnimations()

		// Forward to provider auth modal too — its spinner has its own
		// ID, so the modal's Update will only advance for its own ticks.
		var providerAuthCmd tea.Cmd
		if m.showProviderAuth {
			m.providerAuth, providerAuthCmd = m.providerAuth.Update(tickMsg)
		}

		// Forward to cloud panel so its spinner animates during the
		// loading / awaiting / fetching-me phases. Only when the cloud
		// is showing — outside screenCloud the panel's tick chain
		// would die anyway because the cloud isn't rendering.
		var cloudCmd tea.Cmd
		if m.screen == screenCloud {
			m.cloud, cloudCmd = m.cloud.Update(tickMsg)
		}

		// Forward to session view so its spinner keeps ticking too.
		if m.screen == screenSession && m.sessionView != nil {
			model, sessionCmd := m.sessionView.Update(tickMsg)
			m.sessionView = model.(*SessionViewModel)
			return m, tea.Batch(inboxCmd, providerAuthCmd, cloudCmd, sessionCmd)
		}
		return m, tea.Batch(inboxCmd, providerAuthCmd, cloudCmd)
	}

	// Always keep the spinner ticking, regardless of screen state.
	// The spinner's tick chain is self-sustaining: each handler
	// schedules the next tick. Letting it fall through to the
	// session view delegation would swallow it and permanently kill
	// the spinner.
	//
	// Sidebar session state is push-based via SSE (handlers below), so
	// the legacy 3s polling loop is gone. inboxRefreshMsg now triggers
	// a single one-shot fetch — used by explicit user actions and
	// post-discover resync.
	switch msg := msg.(type) {
	case inboxSSESetupMsg:
		if m.inboxEventsCancel != nil {
			m.inboxEventsCancel()
		}
		m.inboxEventsCancel = msg.cancel
		m.inboxEventsCh = msg.events
		return m, waitForInboxEvent(msg.events)
	case inboxSSEEventMsg:
		changed, cmd := m.applyInboxEvent(msg.event)
		if changed {
			m.refreshSidebarFromCache()
			// Persist after in-place SSE mutations so cached state
			// (e.g. LastReadAt bumped by a MarkRead broadcast) survives
			// TUI exit. Without this, the disk cache only captured
			// snapshots from successful loadDataCmd() returns, so any
			// SSE-driven change between full loads was lost when the
			// process exited — leaving stale unread/status indicators
			// on next launch even though the daemon DB was correct.
			m.persistCacheIfChanged()
		}
		var next tea.Cmd
		if m.inboxEventsCh != nil {
			next = waitForInboxEvent(m.inboxEventsCh)
		}
		if cmd != nil && next != nil {
			return m, tea.Batch(cmd, next)
		}
		if cmd != nil {
			return m, cmd
		}
		return m, next
	case inboxSSEClosedMsg:
		m.inboxEventsCancel = nil
		m.inboxEventsCh = nil
		return m, tea.Batch(m.loadDataCmd(), m.reconnectInboxEvents(msg.delay))
	case inboxSSEErrorMsg:
		if m.inboxEventsCancel != nil {
			m.inboxEventsCancel()
			m.inboxEventsCancel = nil
		}
		m.inboxEventsCh = nil
		return m, m.reconnectInboxEvents(msg.delay)
	case inboxRefreshSubscribeMsg:
		return m, m.subscribeInboxEvents()
	case inboxRefreshMsg:
		_ = msg
		return m, m.loadDataCmd()
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
	// Skip when the sidebar is in text-input mode (creating a new branch)
	// so that space goes to the text input instead.
	voiceInterceptOK := !m.showMerge
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

	// Sidebar-originated control messages have to be handled at the
	// inbox level before the session-view delegation below — otherwise
	// the chat view swallows them and the sidebar's request never lands
	// (e.g. opening a different session while one is already showing).
	switch msg := msg.(type) {
	case sessionSelectedFromSidebarMsg:
		return m, m.openSession(msg.sessionID)
	case sidebarExpandToggledMsg:
		go m.persistSidebarExpanded(m.sidebar.SnapshotExpanded())
		return m, nil
	case composeRequestedMsg:
		// Sidebar's "n" / "Shift+N" gesture. The prefilled worktreePath
		// comes from the cursor's context; an empty string is interpreted
		// as "use the cwd" by openComposingSession. newWorktree opens
		// compose with the New-worktree toggle pre-enabled.
		return m, m.openComposingSession(msg.worktreePath, msg.newWorktree)
	case closeComposeMsg:
		m.closeCompose()
		return m, nil
	case composeSubmittedMsg:
		// The chat view already swapped itself from composing to a
		// live session in-place; this catches the inbox-side state up.
		m.activeConnID = msg.sessionID
		m.sidebar.SetActiveSessionID(msg.sessionID)
		go persistLastSessionForCwd(m.projectDir, msg.sessionID)
		m.clearComposeSnapshot()
		return m, m.loadDataCmd()
	case inboxDataMsg:
		// Always feed the session list to the sidebar, even when the
		// chat view is the active screen. Otherwise restoring straight
		// into a chat on startup leaves the sidebar empty until the
		// user backs out — buildSidebarTree never sees data, so the
		// rail / worktree tree can't render.
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.err = nil
			m.cachedSessions = msg.sessions
			m.sidebar.SetSessions(m.cachedSessions)
			if m.searchQuery == "" {
				m.buildGroups(m.filteredSessions())
			}
			// Persist the snapshot on disk so the next launch can
			// paint instantly. Skip when the signature matches what
			// we last wrote — SSE event bursts shouldn't churn disk.
			m.persistCacheIfChanged()
		}
		return m, nil
	}

	// Menu overlay owns the keyboard while it's open — routed before
	// any screen-specific delegation so the chat / inbox key handlers
	// don't intercept what should be menu navigation.
	if m.showMenu {
		return m.updateMenu(msg)
	}

	// Mouse events route to the sidebar / right pane on every screen, so
	// the sidebar stays clickable wherever it's visible (inbox, chat,
	// settings, cloud) — unless a blocking overlay owns the screen, in
	// which case the event is swallowed rather than reaching the sidebar
	// behind it. Handled before the per-screen blocks below, which each
	// consume every message for their screen.
	switch msg.(type) {
	case tea.MouseClickMsg, tea.MouseReleaseMsg, tea.MouseMotionMsg, tea.MouseWheelMsg:
		if m.anyOverlayOpen() {
			return m, nil
		}
		return m.routeMouse(msg.(tea.MouseMsg))
	}

	// If we're in session detail view (or composing), delegate — but
	// hand key events to the sidebar instead when focus has shifted
	// there (e.g. via left-arrow). Non-key messages (SSE events, ticks,
	// resize) still flow to the session view so its state stays live.
	if m.screen == screenSession && m.sessionView != nil {
		if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
			// Tab swaps panes everywhere — including from inside the
			// chat. Skip when the chat is capturing keys (text input
			// active, or a permission/question prompt awaiting input)
			// so Tab keeps cycling agents / stays out of the prompt.
			chatCapturesKeys := m.pane == paneSessions &&
				(m.sessionView.inputActive || m.sessionView.promptActive())
			if m.showTwoPanes() && key.Matches(normalizeKeyCase(keyMsg), key.NewBinding(key.WithKeys("tab"))) &&
				!chatCapturesKeys {
				if m.pane == paneSidebar {
					m.setPane(paneSessions)
				} else {
					m.setPane(paneSidebar)
				}
				return m, nil
			}
			// In single-pane mode the sidebar is hidden, so its
			// "focus" is invisible — repair m.pane before routing or
			// keys would vanish into a sidebar nobody can see.
			if !m.showTwoPanes() && m.pane == paneSidebar {
				m.setPane(paneSessions)
			}
			if m.pane == paneSidebar {
				return m.handleSidebarKey(keyMsg)
			}
		}
		return m.updateSessionView(msg)
	}

	// If we're on the Settings page, delegate there.
	if m.screen == screenSettings {
		return m.updateSettings(msg)
	}

	// If we're on the Cloud panel, delegate there.
	if m.screen == screenCloud {
		return m.updateCloud(msg)
	}

	// Route background cloud login results to the cloud view even when
	// its panel isn't visible, so the sidebar indicator can flip
	// without the user having to re-enter the panel.
	if _, ok := msg.(cloudLoginResultMsg); ok && m.cloudInitialized {
		var cmd tea.Cmd
		m.cloud, cmd = m.cloud.Update(msg)
		m.sidebar.SetCloudStatus(m.cloud.Status())
		return m, cmd
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

	// If import sessions modal is open, delegate.
	if m.showImportSessions {
		return m.updateImportSessions(msg)
	}

	// If merge overlay is open, delegate.
	if m.showMerge {
		return m.updateMerge(msg)
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
		m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
		if m.showMerge {
			m.mergeOverlay.SetSize(m.width, m.height)
		}
		return m, nil

	case inboxSearchResultMsg:
		// Late-arriving search result after exiting search mode — ignore.
		return m, nil

	case discoverResultMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, m.loadDataCmd()
		}
		noun := "session"
		if msg.imported != 1 {
			noun = "sessions"
		}
		body := fmt.Sprintf("Found %d new %s.\nAdd them to your inbox?", msg.imported, noun)
		if msg.imported == 0 {
			body = "No new sessions found."
		}
		m.showConfirm = true
		m.confirm = newConfirmDialog("Import Sessions", body, "import-done:")
		return m, m.loadDataCmd()

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
				prevBranch := m.sidebar.SelectedBranch()
				if m.pane == paneSessions {
					m.setPane(paneSidebar)
				} else {
					m.setPane(paneSessions)
				}
				if m.sidebar.SelectedBranch() != prevBranch {
					m.applyFiltersAndRebuild()
				}
				return m, nil
			}
			if key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))) {
				prevBranch := m.sidebar.SelectedBranch()
				if m.pane == paneSidebar {
					m.setPane(paneSessions)
				} else {
					m.setPane(paneSidebar)
				}
				if m.sidebar.SelectedBranch() != prevBranch {
					m.applyFiltersAndRebuild()
				}
				return m, nil
			}
		}

		// Route to the focused pane.
		if m.pane == paneSidebar {
			return m.handleSidebarKey(msg)
		}
		return m.handleInboxKey(msg)
	}

	return m, nil
}

func (m *InboxModel) updateSessionView(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case openProviderAuthFromSessionMsg:
		// Tear down the session view (same path as backToInboxMsg) and
		// land the user on the settings page with the provider-auth
		// modal open. The modal lives on InboxModel and overlays the
		// inbox-level view; the session view doesn't render it.
		typed := msg.(openProviderAuthFromSessionMsg)
		if m.activeConnID != "" && m.sessionView != nil {
			draft := strings.TrimSpace(m.sessionView.DraftText())
			go m.client.Session(m.activeConnID).SetDraft(context.Background(), draft)
		}
		if m.activeConnID != "" {
			go m.client.Session(m.activeConnID).MarkRead(context.Background())
		}
		m.replaceSessionView(nil)
		m.activeConnID = ""
		m.openSettings()
		m.providerAuth = newProviderAuthModel(m.client.Host(m.hostname), typed.backend, "")
		m.showProviderAuth = true
		return m, m.providerAuth.Init()

	case backToInboxMsg:
		// Persist any unsent text as a draft before leaving the session.
		if m.activeConnID != "" && m.sessionView != nil {
			draft := strings.TrimSpace(m.sessionView.DraftText())
			go m.client.Session(m.activeConnID).SetDraft(context.Background(), draft)
		}
		closingID := m.activeConnID
		m.screen = screenInbox
		m.replaceSessionView(nil)
		m.activeConnID = ""
		m.sidebar.SetActiveSessionID("")
		// One-shot refresh on return. Do NOT re-seed spinner.Tick: its
		// chain keeps ticking the whole time the user is in the session
		// view (see Update handlers above), so re-seeding would spawn a
		// duplicate chain on every nav round-trip — leading to K
		// parallel pollers after K nav round-trips, which fan out to
		// expensive git subprocesses in clank-host's ListBranches.
		//
		// Mark-read happens *before* the list refresh inside a single
		// command using tea.Sequence so the subsequent List sees the
		// updated LastReadAt. A previous goroutine + tea.Batch raced
		// the List call against the in-flight POST, so the inbox kept
		// rendering the row as unread until the next autoRefresh tick.
		markRead := tea.Cmd(nil)
		if closingID != "" {
			markRead = func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = m.client.Session(closingID).MarkRead(ctx)
				return nil
			}
		}
		return m, tea.Sequence(markRead, m.loadDataCmd())

	case openForkedSessionMsg:
		forkMsg := msg.(openForkedSessionMsg)
		// openSession routes through replaceSessionView, which tears down
		// the previous view's SSE subscription. No explicit cancel needed.
		if m.activeConnID != "" {
			go m.client.Session(m.activeConnID).MarkRead(context.Background())
		}
		// Navigate to the forked session.
		return m, m.openSession(forkMsg.sessionID)

	case tea.WindowSizeMsg:
		// Forward to both.
		wMsg := msg.(tea.WindowSizeMsg)
		m.width = wMsg.Width
		m.height = wMsg.Height
		m.searchInput.SetWidth(m.sessionPaneWidth())
		m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
		model, cmd := m.sessionView.Update(msg)
		m.sessionView = model.(*SessionViewModel)
		return m, cmd

	case tea.KeyPressMsg:
		// Intercept pane-switching shortcuts before they reach the
		// session view — only when it isn't capturing keys: the compose
		// textarea owns typed text, and an active permission/question
		// prompt owns its reply keys ("n" must deny, not open compose;
		// plan-revision/"Other" typing must not toggle panes).
		if m.sessionView != nil && !m.sessionView.inputActive && !m.sessionView.promptActive() {
			raw := msg.(tea.KeyPressMsg)
			// Compose a new session prefilled with the current chat's
			// worktree — same gesture as "n"/"Shift+N" in the sidebar, but
			// the context is the chat. Shift+N (checked on the raw key,
			// before case folding) pre-enables the New-worktree toggle.
			if isShiftN(raw) {
				worktreeDir := ""
				if m.sessionView.info != nil {
					worktreeDir = m.sessionView.info.GitRef.LocalPath
				}
				return m, m.openComposingSession(worktreeDir, true)
			}
			k := normalizeKeyCase(raw)
			switch {
			case key.Matches(k, key.NewBinding(key.WithKeys("left", "shift+left"))):
				if m.showTwoPanes() {
					m.setPane(paneSidebar)
					return m, nil
				}
			case key.Matches(k, key.NewBinding(key.WithKeys("w"))):
				return m, m.toggleSidebarFromKey()
			case key.Matches(k, key.NewBinding(key.WithKeys("n"))):
				worktreeDir := ""
				if m.sessionView.info != nil {
					worktreeDir = m.sessionView.info.GitRef.LocalPath
				}
				return m, m.openComposingSession(worktreeDir, false)
			}
		}
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

func (m *InboxModel) updateImportSessions(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case importSessionsCancelMsg:
		m.showImportSessions = false
		return m, nil

	case importSessionsConfirmMsg:
		m.showImportSessions = false
		if len(msg.providers) == 0 {
			return m, nil
		}
		return m, m.discoverForProvidersCmd(msg.providers)

	default:
		var cmd tea.Cmd
		m.importSessions, cmd = m.importSessions.Update(msg)
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
			return m, m.loadDataCmd()
		}
		return m, nil

	default:
		cmd := m.mergeOverlay.Update(msg)
		return m, cmd
	}
}

// openComposingSession opens a composing SessionViewModel where the
// user types their first prompt. The session is created on send. The
// caller supplies the target worktree directory — pulled from context
// (cursor's worktree, active session's worktree, or cwd) so the user
// almost never has to override it. Empty worktreeDir falls back to
// the cwd, treating it the same as "this project."
//
// Compose behaves as an overlay over the previous right-pane state:
// the screen / session / pane / sidebar active row are snapshotted so
// closeComposeMsg can restore them on cancel. Re-entering compose
// while already in compose preserves the original snapshot.
func (m *InboxModel) openComposingSession(worktreeDir string, newWorktree bool) tea.Cmd {
	if !m.activeCompose {
		m.activeCompose = true
		m.preComposeScreen = m.screen
		m.preComposeSession = m.sessionView
		m.preComposeConnID = m.activeConnID
		m.preComposePane = m.pane
		m.preComposeActiveRow = m.activeConnID
	}

	m.screen = screenSession
	m.activeConnID = ""
	m.sidebar.SetActiveSessionID("")
	m.setPane(paneSessions)

	if worktreeDir == "" {
		worktreeDir = m.projectDir
	}

	// NOTE: deliberately not routing through replaceSessionView here.
	// preComposeSession snapshotted m.sessionView above; closeCompose
	// restores it and expects its SSE subscription to still be live.
	m.sessionView = NewSessionViewComposing(m.client, worktreeDir)
	m.sessionView.isNewWorktree = newWorktree
	m.sessionView.voice = &m.voice
	m.sessionView.width = m.width
	m.sessionView.height = m.height
	if m.width > 0 {
		m.sessionView.input.SetWidth(m.width - promptInputBorderSize)
	}
	return m.sessionView.Init()
}

// closeCompose restores the right pane to whatever it was showing
// before openComposingSession was called. Called when the user
// dismisses compose with Esc (closeComposeMsg) — submitting a prompt
// clears the snapshot via clearComposeSnapshot instead, since the
// new live session takes over.
func (m *InboxModel) closeCompose() {
	if !m.activeCompose {
		return
	}
	m.screen = m.preComposeScreen
	m.sessionView = m.preComposeSession
	m.activeConnID = m.preComposeConnID
	m.sidebar.SetActiveSessionID(m.preComposeActiveRow)
	m.setPane(m.preComposePane)
	m.clearComposeSnapshot()
}

// clearComposeSnapshot drops the saved pre-compose state and exits
// "in compose" mode. Called both on Esc (after restoring) and on
// successful submit (when the new live session is the desired state
// going forward, not the pre-compose one).
func (m *InboxModel) clearComposeSnapshot() {
	m.activeCompose = false
	m.preComposeScreen = 0
	m.preComposeSession = nil
	m.preComposeConnID = ""
	m.preComposePane = paneSessions
	m.preComposeActiveRow = ""
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

	// Filter by repo identity (canonical GitRef). Sessions without a
	// GitRef (e.g. adopted backends with no origin remote) are dropped
	// from the project view since they can't be attributed to this repo.
	if m.projectFilter && (m.gitRef.LocalPath != "" || m.gitRef.WorktreeID != "") {
		filtered := make([]agent.SessionInfo, 0, len(sessions))
		for _, s := range sessions {
			if agent.RepoKey(s.GitRef) == agent.RepoKey(m.gitRef) {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	// Filter by worktree directory (if a worktree is selected in the sidebar).
	// Match by LocalPath so discovered sessions (which only have LocalPath set)
	// are correctly included.
	// TODO: investigate whether WorktreeBranch field can be deprecated.
	if dir := m.sidebar.SelectedWorktreeDir(); dir != "" {
		filtered := make([]agent.SessionInfo, 0, len(sessions))
		for _, s := range sessions {
			if s.GitRef.LocalPath == dir {
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
		m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
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
	// Apply client-side structured filters (e.g. project, branch) on
	// top of the daemon's text search results so the search view
	// honours the same sidebar selection as the unfiltered list.
	if m.projectFilter && (m.gitRef.LocalPath != "" || m.gitRef.WorktreeID != "") {
		filtered := make([]agent.SessionInfo, 0, len(sessions))
		for _, s := range sessions {
			if agent.RepoKey(s.GitRef) == agent.RepoKey(m.gitRef) {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}
	if dir := m.sidebar.SelectedWorktreeDir(); dir != "" {
		filtered := make([]agent.SessionInfo, 0, len(sessions))
		for _, s := range sessions {
			if s.GitRef.LocalPath == dir {
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
	// Shift+N composes a new session on a fresh worktree. Checked before
	// normalizeKeyCase folds the uppercase "N" into a plain "n".
	if isShiftN(msg) {
		return m, m.openComposingSession("", true)
	}
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
		// From the inbox right pane, "n" composes a new session in
		// the cwd's worktree — there's no per-row worktree context
		// to prefill from here. (Shift+N is handled at the top.)
		return m, m.openComposingSession("", false)
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
	case key.Matches(msg, key.NewBinding(key.WithKeys("left", "shift+left"))):
		// Left arrow navigates to the sidebar when it's visible.
		if m.showTwoPanes() {
			prevBranch := m.sidebar.SelectedBranch()
			m.setPane(paneSidebar)
			if m.sidebar.SelectedBranch() != prevBranch {
				m.applyFiltersAndRebuild()
			}
			return m, nil
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("w"))):
		return m, m.toggleSidebarFromKey()
	case key.Matches(msg, key.NewBinding(key.WithKeys("?"))):
		m.showHelp = true
		return m, nil
	}
	return m, nil
}

func (m *InboxModel) openSession(sessionID string) tea.Cmd {
	m.screen = screenSession
	m.activeConnID = sessionID
	m.sidebar.SetActiveSessionID(sessionID)
	// Persist "this is the session we were on" so next launch from
	// the same cwd picks up here.
	go persistLastSessionForCwd(m.projectDir, sessionID)

	// Mark session as read so the inbox reflects the change immediately.
	go m.client.Session(sessionID).MarkRead(context.Background())

	// Pre-subscribe to SSE before creating the view model to avoid missing
	// events from an already-busy session. The connect is to a local Unix
	// socket so it completes near-instantly.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	events, err := m.client.Sessions().Subscribe(sseCtx)

	next := NewSessionViewModel(m.client, sessionID)
	next.voice = &m.voice
	if err == nil {
		next.SetEventChannel(events, sseCancel)
	} else {
		// Fall back to subscribing in Init() if pre-subscribe fails.
		sseCancel()
	}
	m.replaceSessionView(next)

	// Intentionally NOT calling setPane(paneSessions) here. Opening a
	// session from the sidebar swaps the right pane's content but does
	// NOT yank focus out of the sidebar — the user can keep arrow-
	// keying through other sessions and Enter on each one to preview
	// it in the right pane. Right-arrow is the explicit "go into this
	// chat" gesture; that's where focus actually moves.
	//
	// The active-session rail in the sidebar marks which row is the
	// one currently rendered in the right pane.

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

// replaceSessionView swaps m.sessionView for next, cancelling the previous
// view's SSE subscription first. Every site that reassigns m.sessionView to
// a freshly-constructed view must go through here — otherwise the prior
// view's waitForEvent goroutine leaks and keeps delivering sessionEventMsg
// values into the Bubble Tea queue, which then land in the wrong (newly
// active) view. See TestReplaceSessionView_CancelsPreviousSubscription.
func (m *InboxModel) replaceSessionView(next *SessionViewModel) {
	if m.sessionView != nil && m.sessionView.cancelEvents != nil {
		m.sessionView.cancelEvents()
		m.sessionView.cancelEvents = nil
	}
	m.sessionView = next
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
		if err := m.client.Session(sessionID).Delete(ctx); err != nil {
			return inboxDataMsg{err: err}
		}
		// Reload data after delete.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		sessions, err := m.client.Sessions().List(ctx2)
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
	case "import-done":
		// User confirmed the import summary — just reload.
		return m.loadDataCmd()
	}
	return nil
}

func (m *InboxModel) setSessionVisibility(sessionID string, visibility agent.SessionVisibility) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.client.Session(sessionID).SetVisibility(ctx, visibility); err != nil {
			return inboxDataMsg{err: err}
		}
		// Reload data after visibility change.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		sessions, err := m.client.Sessions().List(ctx2)
		return inboxDataMsg{sessions: sessions, err: err}
	}
}

func (m *InboxModel) toggleFollowUp(sessionID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := m.client.Session(sessionID).ToggleFollowUp(ctx); err != nil {
			return inboxDataMsg{err: err}
		}
		// Reload data so the session moves to/from the FOLLOW UP group.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		sessions, err := m.client.Sessions().List(ctx2)
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

// renderStatusLine returns a single-line global status to overlay at
// the bottom of the screen. Visible from every screen so errors and
// notices don't get trapped inside the inbox view's renderSessionPane.
// Returns "" when nothing's happening. Priority: error > notice.
func (m *InboxModel) renderStatusLine() string {
	if m.err != nil {
		return lipgloss.NewStyle().Foreground(dangerColor).Render("✗ " + m.err.Error())
	}
	if m.notice != "" {
		return lipgloss.NewStyle().Foreground(successColor).Render("✓ " + m.notice)
	}
	return ""
}

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
	if m.width == 0 {
		v := newVoiceEnabledView("Loading...")
		return v
	}

	// Compute right-pane content as a styled string. The screen enum
	// picks which sub-model renders into the right pane; the sidebar
	// stays on the left in every case.
	var (
		sessionContent string
		sessionOnMouse func(tea.MouseMsg) tea.Cmd
		sessionCursor  *tea.Cursor
	)
	switch m.screen {
	case screenSession:
		if m.sessionView != nil {
			m.sizeSessionViewForRightPane()
			v := m.sessionView.View()
			sessionContent = v.Content
			sessionOnMouse = v.OnMouse
			sessionCursor = v.Cursor
		} else {
			sessionContent = m.renderSessionPane()
		}
	case screenSettings:
		sessionContent = m.settings.View()
	case screenCloud:
		sessionContent = m.cloud.View()
	default:
		sessionContent = m.renderSessionPane()
	}

	var content string

	if m.showTwoPanes() {
		sidebarView := m.sidebar.View()
		// The chat view skips the outer pane border — every message
		// already paints its own rounded border when selected, so a
		// wrapping border would double up. Other screens (inbox /
		// settings / cloud) keep the focus-aware border so the user
		// knows where keys land.
		var rightPane string
		if m.screen == screenSession {
			rightPane = sessionContent
		} else {
			rightPane = m.rightPaneBorder().Render(sessionContent)
		}
		content = lipgloss.JoinHorizontal(lipgloss.Top, sidebarView, " ", rightPane)
		// Measure the sidebar's *rendered* width (lipgloss includes the
		// border) rather than the requested width — the border inset
		// makes them differ, and the chat is placed right after this
		// block plus the gap column. chatPaneXOffset() returns the same
		// value so click translation matches where the caret is drawn.
		m.chatPaneX = lipgloss.Width(sidebarView) + sidebarGap
	} else {
		content = sessionContent
		m.chatPaneX = 0
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

	// Overlay theme picker if open (shown above everything else so it's
	// unambiguous that the background palette is a live preview).
	if m.showThemePicker {
		content = overlayCenter(content, m.themePicker.View(), m.width, m.height)
	}

	// Overlay provider auth modal if open.
	if m.showProviderAuth {
		content = overlayCenter(content, m.providerAuth.View(), m.width, m.height)
	}

	// Overlay cloud URL picker modal if open.
	if m.showCloudURLPicker {
		content = overlayCenter(content, m.cloudURLPicker.View(), m.width, m.height)
	}

	// Overlay import sessions modal if open.
	if m.showImportSessions {
		content = overlayCenter(content, m.importSessions.View(), m.width, m.height)
	}

	// Overlay help if open.
	if m.showHelp {
		content = m.overlayHelp(content)
	}

	// Overlay Kitty keyboard warning if shown.
	if m.showKittyWarning {
		content = m.overlayKittyWarning(content)
	}

	// Overlay a single-line global status when something is in flight
	// or a recent action produced a notice/error. Visible from every
	// screen (chat / inbox / settings / cloud) so feedback isn't
	// trapped inside the inbox view's renderSessionPane.
	if status := m.renderStatusLine(); status != "" {
		content = overlayBottom(content, status, m.width, m.height)
	}

	// Mouse reporting is a single terminal-level toggle: it must be
	// declared on the outer (rendered) view, otherwise wheel/click
	// events never reach us and the terminal falls back to translating
	// wheel into arrow keys. Enable it whenever the sidebar is visible
	// (so its rows are clickable on every screen) or a chat is open (for
	// click-to-select and wheel scroll). The cost is that native mouse
	// text-selection then needs Option held on those screens — the same
	// as it already worked in the chat.
	var v tea.View
	if m.showTwoPanes() || (m.screen == screenSession && m.sessionView != nil) {
		v = newVoiceEnabledViewWithMouse(content)
	} else {
		v = newVoiceEnabledView(content)
	}

	// When the chat view is rendered inside the right pane, propagate
	// its mouse handler so selection / copy still work — but translate
	// X coordinates so the chat view sees them relative to its own
	// origin (the sidebar gap shifts the right pane by that amount).
	if sessionOnMouse != nil {
		offset := m.chatPaneX
		v.OnMouse = func(msg tea.MouseMsg) tea.Cmd {
			translated := offsetMouseX(msg, -offset)
			return sessionOnMouse(translated)
		}
	}
	if sessionCursor != nil {
		// Same offset for the cursor: chat view positions it relative
		// to its own pane, the outer view places it in the composite.
		shifted := *sessionCursor
		shifted.X += m.chatPaneX
		v.Cursor = &shifted
	}
	return v
}

// sizeSessionViewForRightPane keeps the embedded chat view in sync
// with the current right-pane allocation. Called once per View() so a
// terminal resize, a sidebar width adjustment, or a sidebar toggle all
// reach the chat view before its next render.
//
// Width is chatPaneContentWidth — the borderless chat fills the space
// between the sidebar and the terminal's right edge (minus a wrap-safety
// gutter), rather than reserving columns for an outer pane border it never
// draws. The chat's input box renders its own border at this width and still
// clears the pane separator because the gutter keeps it inside the frame.
func (m *InboxModel) sizeSessionViewForRightPane() {
	if m.sessionView == nil {
		return
	}
	w := m.chatPaneContentWidth()
	h := m.height - paneBorderInset
	if w < 10 {
		w = 10
	}
	if h < 5 {
		h = 5
	}
	m.sessionView.width = w
	m.sessionView.height = h
	if m.sessionView.input.Width() != w-promptInputBorderSize {
		m.sessionView.input.SetWidth(w - promptInputBorderSize)
	}
}

// offsetMouseX returns a copy of msg with the underlying Mouse.X shifted
// by dx. Used to translate composite-view mouse coordinates into a
// sub-view's local frame before forwarding the event.
func offsetMouseX(msg tea.MouseMsg, dx int) tea.MouseMsg {
	switch typed := msg.(type) {
	case tea.MouseClickMsg:
		typed.X += dx
		return typed
	case tea.MouseReleaseMsg:
		typed.X += dx
		return typed
	case tea.MouseMotionMsg:
		typed.X += dx
		return typed
	case tea.MouseWheelMsg:
		typed.X += dx
		return typed
	default:
		return msg
	}
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
		sb.WriteString(renderError(m.err, m.width))
		sb.WriteString("\n\n")
	}

	// Notice (transient success message).
	if m.notice != "" {
		sb.WriteString(lipgloss.NewStyle().Foreground(successColor).Render("✓ " + m.notice))
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
		parts := []string{"enter: open", "n: new", "/: search", "w: worktrees", "?: help", "q: quit"}
		help = helpStyle.Render(strings.Join(parts, " | "))
	}
	sb.WriteString(help)

	return sb.String()
}

// --- Two-pane layout helpers ---

// minTwoPaneWidth is the minimum terminal width to show both panes.
// Below this, only the session pane is shown.
const minTwoPaneWidth = 80

// sidebarGap is the number of blank columns between the sidebar and the
// session pane in two-pane layout.
const sidebarGap = 1

// defaultSidebarWidthRatio is the default sidebar width as a percentage of
// the terminal width. Adjustable at runtime with the +/- keys.
const defaultSidebarWidthRatio = 40

// showTwoPanes returns true when the terminal is wide enough and the user
// hasn't manually hidden the sidebar with 'w'.
func (m *InboxModel) showTwoPanes() bool {
	return m.width >= minTwoPaneWidth && !m.sidebarHidden
}

// setPane is the single point of truth for which top-level pane has
// keyboard focus. It keeps SidebarModel.focused and SessionViewModel
// .paneFocused in sync with m.pane so callers can't drift them apart
// (which would silently break the focus border on either pane, or
// leave the chat's message cursor highlighted while the sidebar is
// where the user is actually navigating).
func (m *InboxModel) setPane(p inboxPane) {
	m.pane = p
	m.sidebar.SetFocused(p == paneSidebar)
	if m.sessionView != nil {
		m.sessionView.paneFocused = p == paneSessions
	}
}

// focusActiveChatMessageMode moves focus to the chat pane and engages
// message mode on the active session view. Returns nil when no session
// is currently shown so callers can chain it without guard duplication.
// This is the second-step gesture: Enter previews a session (no focus
// change), "m" commits and starts typing.
func (m *InboxModel) focusActiveChatMessageMode() tea.Cmd {
	if m.screen != screenSession || m.sessionView == nil {
		return nil
	}
	m.setPane(paneSessions)
	return m.sessionView.enterMessageMode()
}

// toggleSidebar flips sidebar visibility without stealing focus.
// Inverse property: two consecutive toggles must return to the original
// pane focus. Focus only moves when the currently-focused pane is being
// hidden (no focus on invisible pane), and is restored on un-hide.
func (m *InboxModel) toggleSidebar() tea.Cmd {
	m.sidebarHidden = !m.sidebarHidden
	if m.sidebarHidden {
		m.sidebarWasFocused = m.pane == paneSidebar
		if m.sidebarWasFocused {
			m.setPane(paneSessions)
		}
	} else if m.sidebarWasFocused {
		m.setPane(paneSidebar)
		m.sidebarWasFocused = false
	}
	// Force a full repaint. Toggling reshapes the layout (single block vs
	// sidebar+chat) and reflows wrapped lines, shifting rows; without an
	// explicit clear the terminal can keep stale cells from the previous frame
	// — ghost borders and phantom text, most visible on narrow panes where the
	// chat wraps heavily.
	return tea.ClearScreen
}

// toggleSidebarFromKey flips sidebar visibility and persists the new state.
// Persistence lives here rather than in toggleSidebar so tests can drive the
// pure toggle without writing the preferences file.
func (m *InboxModel) toggleSidebarFromKey() tea.Cmd {
	cmd := m.toggleSidebar()
	go persistSidebarHidden(m.sidebarHidden)
	return cmd
}

// sidebarWidthRatioFromPrefs returns the persisted sidebar width ratio, or the
// default if none has been saved yet.
func sidebarWidthRatioFromPrefs(prefs config.Preferences) int {
	if prefs.SidebarWidthRatio > 0 {
		return prefs.SidebarWidthRatio
	}
	return defaultSidebarWidthRatio
}

// persistSidebarWidthRatio saves the given ratio to the preferences file.
// Intended to be called in a goroutine so the TUI doesn't block on disk
// I/O. The ratio is passed in (rather than read from m.sidebarWidthRatio
// inside the goroutine) so concurrent main-loop updates don't race with
// the disk write. Receiver is kept for symmetry with other InboxModel
// methods.
func (m *InboxModel) persistSidebarWidthRatio(ratio int) {
	_ = config.UpdatePreferences(func(p *config.Preferences) { p.SidebarWidthRatio = ratio })
}

// persistSidebarHidden saves the sidebar's open/closed toggle state.
// Called in a goroutine so the TUI doesn't block on disk I/O; the value
// is passed in so concurrent main-loop toggles don't race the write.
func persistSidebarHidden(hidden bool) {
	_ = config.UpdatePreferences(func(p *config.Preferences) { p.SidebarHidden = hidden })
}

// persistSidebarExpanded writes the sidebar's expand-state snapshot to
// the preferences file. Called from a goroutine after every toggle so
// the TUI doesn't block on disk I/O. The snapshot is captured by the
// main loop so concurrent writes can't race with the save.
func (m *InboxModel) persistSidebarExpanded(snapshot map[string]bool) {
	_ = config.UpdatePreferences(func(p *config.Preferences) { p.SidebarExpanded = snapshot })
}

// persistLastSessionForCwd records the session id that was open most
// recently from this cwd. Fire-and-forget; failures are silently
// ignored — "restore the last session" is a convenience, not a
// correctness guarantee.
func persistLastSessionForCwd(cwd, sessionID string) {
	_ = config.SetLastSessionForCwd(cwd, sessionID)
}

// sidebarRenderWidth returns the width allocated to the sidebar (including border).
// The width scales with the terminal: sidebarWidthRatio% of screen width, clamped to [22, 60].
// Falls back to the sidebarWidth constant when the screen width is not yet known.
func (m *InboxModel) sidebarRenderWidth() int {
	if m.width > 0 {
		w := m.width * m.sidebarWidthRatio / 100
		if w < 22 {
			w = 22
		}
		if w > 60 {
			w = 60
		}
		return w
	}
	return sidebarWidth
}

// sessionPaneWidth returns the inner content width of the session pane
// (excluding its focus border and a small wrap-safety buffer in two-pane
// mode). Callers that build content lines must use this value; the
// bordered Style's Width adds paneWrapBuffer back so the visible bordered
// area fills the allocated space.
func (m *InboxModel) sessionPaneWidth() int {
	if m.showTwoPanes() {
		return m.width - m.sidebarRenderWidth() - sidebarGap - paneBorderInset - paneWrapBuffer
	}
	return m.width
}

// chatPaneContentWidth is the width available to the borderless chat/session
// view. Unlike sessionPaneWidth — sized for the bordered inbox/settings/cloud
// panes — the chat paints no outer pane border (every message draws its own),
// so it keeps only paneWrapBuffer as a right-edge gutter against the terminal.
// This is what lets the chat fill the horizontal space instead of leaving the
// border's worth of columns empty.
//
// In two-pane mode it subtracts the sidebar's *rendered* width, not its
// allocation: sidebarRenderWidth is the allocation, but lipgloss draws the
// sidebar paneBorderInset narrower (v2 folds the border into Width) and
// chatPaneX places the chat at that actual edge. Mirroring that keeps the chat
// flush to the same right edge as single-pane mode instead of stopping
// paneBorderInset columns short.
func (m *InboxModel) chatPaneContentWidth() int {
	if m.showTwoPanes() {
		sidebarRendered := m.sidebarRenderWidth() - paneBorderInset
		return m.width - sidebarRendered - sidebarGap - paneWrapBuffer
	}
	return m.width - paneWrapBuffer
}

// chatPaneXOffset returns the X coordinate (in the outer terminal frame)
// of the chat's leftmost cell. Used to translate mouse X into the chat's
// local frame so click hit-tests target the right column. Returns the
// value cached by the last View(), measured from the sidebar's actual
// rendered width — so click translation and where the caret is drawn use
// one source of truth and can't drift (the sidebar's border inset makes
// its rendered width smaller than sidebarRenderWidth()).
func (m *InboxModel) chatPaneXOffset() int {
	if !m.showTwoPanes() {
		return 0
	}
	return m.chatPaneX
}

// mouseInSidebar reports whether the mouse event hit the sidebar pane
// (X is left of the chat's leftmost cell). In single-pane mode the
// sidebar isn't visible, so this is always false and every event
// reaches the chat.
func (m *InboxModel) mouseInSidebar(msg tea.MouseMsg) bool {
	if !m.showTwoPanes() {
		return false
	}
	return msg.Mouse().X < m.chatPaneXOffset()
}

// routeMouse dispatches a mouse event to the pane it belongs to, on any
// screen. Press → motion* → release stays with the pane that received
// the press (mouseDragOwner pins the drag) so a boundary-crossing drag
// doesn't half-execute and leak selection state. Clicks/wheels outside a
// drag route by hover (web-like): interacting with a pane focuses it.
// Sidebar events drive the sidebar regardless of which screen owns the
// right pane; right-pane events go to the chat (when one is open) or,
// on other screens, scroll the list.
func (m *InboxModel) routeMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.mouseDragOwner != nil {
		owner := *m.mouseDragOwner
		if _, isRelease := msg.(tea.MouseReleaseMsg); isRelease {
			m.mouseDragOwner = nil
		}
		switch owner {
		case paneSessions:
			return m.forwardMouseToRightPane(msg)
		case paneSidebar:
			// Sidebar has no drag semantics; swallow motion/release
			// until the press's release arrives so the chat doesn't
			// see an orphan release at the dragged-to X.
			return m, nil
		}
	}

	inSidebar := m.mouseInSidebar(msg)

	switch typed := msg.(type) {
	case tea.MouseClickMsg:
		if inSidebar {
			if typed.Button == tea.MouseLeft {
				m.setPane(paneSidebar)
				owner := paneSidebar
				m.mouseDragOwner = &owner
				return m.handleSidebarClick(typed)
			}
			return m, nil
		}
		if typed.Button == tea.MouseLeft {
			owner := paneSessions
			m.mouseDragOwner = &owner
			m.setPane(paneSessions)
		}
		return m.forwardMouseToRightPane(msg)

	case tea.MouseWheelMsg:
		if inSidebar {
			m.setPane(paneSidebar)
			m.sidebar.HandleWheel(typed.Button)
			return m, nil
		}
		m.setPane(paneSessions)
		return m.forwardMouseToRightPane(msg)

	default:
		// Motion or release without a tracked press. Drop sidebar-side
		// events (no hover behavior); forward right-pane-side so any
		// stray chat handler still sees consistent coordinates.
		if inSidebar {
			return m, nil
		}
		return m.forwardMouseToRightPane(msg)
	}
}

// forwardMouseToRightPane delivers a right-pane mouse event to whatever
// owns the right pane. On the chat screen the event goes to the session
// view with X translated into its local frame (Y is shared, so no Y
// shift). On other screens only wheel is honored — re-dispatched as
// up/down so list scrolling survives now that mouse reporting is forced
// on for the sidebar; right-pane clicks there are ignored (those panes
// aren't click-driven yet).
func (m *InboxModel) forwardMouseToRightPane(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.screen == screenSession && m.sessionView != nil {
		translated := offsetMouseX(msg, -m.chatPaneXOffset())
		model, cmd := m.sessionView.Update(translated)
		m.sessionView = model.(*SessionViewModel)
		return m, cmd
	}
	if wheel, ok := msg.(tea.MouseWheelMsg); ok {
		return m.scrollRightPaneByWheel(wheel)
	}
	return m, nil
}

// scrollRightPaneByWheel maps a wheel tick to an up/down key command so
// inbox/settings/cloud list scrolling works while mouse reporting is on.
func (m *InboxModel) scrollRightPaneByWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseWheelUp:
		return m, func() tea.Msg { return tea.KeyPressMsg{Code: tea.KeyUp} }
	case tea.MouseWheelDown:
		return m, func() tea.Msg { return tea.KeyPressMsg{Code: tea.KeyDown} }
	default:
		return m, nil
	}
}

// anyOverlayOpen reports whether a blocking overlay (menu, dialog,
// picker, help) currently owns the screen. Sidebar mouse routing is
// suppressed while one is open so a click doesn't fall through to the
// sidebar behind it.
func (m *InboxModel) anyOverlayOpen() bool {
	return m.showMenu || m.showConfirm || m.showMerge || m.showThemePicker ||
		m.showProviderAuth || m.showCloudURLPicker || m.showImportSessions || m.showHelp ||
		m.showKittyWarning
}

// handleSidebarClick resolves a left click in the sidebar to the row
// under the pointer, moves the selection there, and activates it — the
// same outcome as pressing Enter on that row. A click on a border,
// blank, or padding gap resolves to no node and is ignored.
func (m *InboxModel) handleSidebarClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	idx := m.sidebar.NodeAtRow(msg.Y)
	if idx < 0 {
		return m, nil
	}
	m.sidebar.SetCursor(idx)
	return m.activateSidebarCursor()
}

// activateSidebarCursor performs the activation for the node under the
// sidebar cursor — the shared behavior behind both Enter and a left
// click. Footer rows open their panel; a session opens in the chat; a
// worktree or "show more" bucket toggles its expand state.
func (m *InboxModel) activateSidebarCursor() (tea.Model, tea.Cmd) {
	switch {
	case m.sidebar.CursorOnAllSessions():
		return m.showInboxList()
	case m.sidebar.CursorOnSettings():
		m.openSettings()
		return m, nil
	case m.sidebar.CursorOnCloud():
		return m, m.openCloud()
	case m.sidebar.CursorOnImport():
		m.showImportSessions = true
		m.importSessions = newImportSessionsModel()
		return m, nil
	default:
		// Session → emit sessionSelectedFromSidebarMsg (opens the chat);
		// worktree / Older bucket → toggle expand.
		return m, m.sidebar.handleEnter()
	}
}

// showInboxList returns the right pane to the date-grouped inbox (the
// "All sessions" view). An open session is torn down via backToInboxMsg
// so its draft persists and its poller stops; a Cloud/Settings preview
// just drops back to the list.
func (m *InboxModel) showInboxList() (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenSession:
		return m, func() tea.Msg { return backToInboxMsg{} }
	case screenCloud:
		m.cloud.SetFocused(false)
	case screenSettings:
		m.settings.SetFocused(false)
	}
	m.screen = screenInbox
	return m, nil
}

// rightPaneBorder returns the bordered Style used by View() to wrap the
// right pane in two-pane mode. The Style's Width is sessionPaneWidth() +
// paneWrapBuffer — strictly greater than what renderRow produces, so
// lipgloss does not wrap content lines at the boundary. Exposed as a
// method (rather than inlined in View()) so tests can verify the
// no-wrap invariant against the same code path the UI uses.
func (m *InboxModel) rightPaneBorder() lipgloss.Style {
	return paneBorderStyle(m.pane == paneSessions).
		Width(m.sessionPaneWidth() + paneWrapBuffer).
		Height(m.height - paneBorderInset)
}

// handleSidebarKey forwards key events to the sidebar while it's focused.
// Global keys (q, ctrl+c) are still handled at the inbox level.
func (m *InboxModel) handleSidebarKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	msg = normalizeKeyCase(msg)

	// Global keys handled even when sidebar is focused.
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		m.cleanupVoice()
		return m, tea.Quit
	case key.Matches(msg, key.NewBinding(key.WithKeys("q"))):
		m.cleanupVoice()
		return m, tea.Quit
	case key.Matches(msg, key.NewBinding(key.WithKeys("w"))):
		return m, m.toggleSidebarFromKey()
	case key.Matches(msg, key.NewBinding(key.WithKeys("?"))):
		m.showHelp = true
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("m"))):
		// Prefer the "focus active chat + enter message mode" gesture
		// when an active session is shown in the right pane: the user
		// just pressed Enter on a session in the sidebar (which only
		// previews) and now wants to commit and start typing. The
		// branch-merge gesture stays the fallback for when no session
		// is active (e.g. on a branch row before opening anything).
		if m.screen == screenSession && m.activeConnID != "" && m.sessionView != nil {
			return m, m.focusActiveChatMessageMode()
		}
		bi := m.sidebar.SelectedBranchInfo()
		if bi != nil && !bi.IsDefault {
			m.mergeOverlay = newMergeOverlay(m.client, m.hostname, m.gitRef, *bi)
			m.mergeOverlay.SetSize(m.width, m.height)
			m.showMerge = true
		}
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("+", "="))):
		// Increase sidebar width by one character.
		if m.width > 0 {
			target := m.sidebarRenderWidth() + 1
			newRatio := target * 100 / m.width
			if newRatio <= m.sidebarWidthRatio {
				newRatio = m.sidebarWidthRatio + 1
			}
			m.sidebarWidthRatio = newRatio
			m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
			go m.persistSidebarWidthRatio(m.sidebarWidthRatio)
		}
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("-"))):
		// Decrease sidebar width by one character.
		if m.width > 0 {
			target := m.sidebarRenderWidth() - 1
			newRatio := target * 100 / m.width
			if newRatio >= m.sidebarWidthRatio {
				newRatio = m.sidebarWidthRatio - 1
			}
			if newRatio < 1 {
				newRatio = 1
			}
			m.sidebarWidthRatio = newRatio
			m.sidebar.SetSize(m.sidebarRenderWidth(), m.height)
			go m.persistSidebarWidthRatio(m.sidebarWidthRatio)
		}
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("right", "shift+right"))):
		// Settings / Cloud rows: right arrow opens their preview panel
		// (mirrors Enter).
		if m.sidebar.CursorOnSettings() {
			m.openSettings()
			return m, nil
		}
		if m.sidebar.CursorOnCloud() {
			return m, m.openCloud()
		}
		// Default: shift focus to whatever is in the right pane (inbox
		// or session view). Mirrors the pre-tree behavior where left/
		// right just toggle which pane is focused.
		m.setPane(paneSessions)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		// Enter activates the row under the sidebar cursor — the same
		// behavior as a left click (see activateSidebarCursor): footer
		// rows open their panel, a session opens in the chat, a worktree
		// or "show more" bucket toggles its expand state.
		return m.activateSidebarCursor()
	}

	// Track branch selection before and after to detect changes.
	prevBranch := m.sidebar.SelectedBranch()
	cmd := m.sidebar.Update(msg)
	if m.sidebar.SelectedBranch() != prevBranch {
		m.applyFiltersAndRebuild()
	}
	// Hover preview: keep the right pane in sync with the sidebar
	// cursor. Cloud / Settings rows show their respective panels;
	// any other row snaps back to the inbox. Unified here so navigation
	// like Settings → Cloud (cursor moving across both footer rows)
	// transitions cleanly in one pass instead of stalling on whichever
	// screen happened to be showing.
	switch {
	case m.sidebar.CursorOnCloud():
		if m.screen != screenCloud {
			// Batch the cloud's Init Cmd (loads prefs, fires /me if
			// a saved session exists) into the sidebar dispatch's
			// return so the panel's async work runs without the user
			// having to press Enter first.
			cmd = tea.Batch(cmd, m.showCloud())
		}
	case m.sidebar.CursorOnSettings():
		if m.screen != screenSettings {
			m.showSettings()
		}
	default:
		// Cursor moved off both Cloud and Settings — drop back to
		// inbox if we were previewing either.
		if m.screen == screenCloud {
			m.screen = screenInbox
			m.cloud.SetFocused(false)
		} else if m.screen == screenSettings {
			m.screen = screenInbox
			m.settings.SetFocused(false)
		}
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
			emptyMsg = "No sessions. Press 'n' to start a new session, or run 'clank please <prompt>'."
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
	ago := TimeAgo(s.UpdatedAt)
	stateIcon := styledAgentStatus(s.Status, m.spinner.View())

	unreadMark := sessionMarker(*s)

	// Branch badge — shown only when viewing all branches (no branch filter active).
	const branchBadgeWidth = 12
	branchBadge := ""
	branchExtra := 0
	if m.sidebar.SelectedBranch() == "" && s.GitRef.WorktreeBranch != "" {
		branchLabel := s.GitRef.WorktreeBranch
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

	// Project badge — shown only in "All" view; redundant when a specific folder is selected.
	const projectBadgeWidth = 13 // 12 chars + 1 trailing space
	styledProject := ""
	isAllView := m.sidebar.SelectedBranch() == ""
	if isAllView {
		projectName := agent.RepoDisplayName(s.GitRef)
		if len(projectName) > 12 {
			projectName = projectName[:11] + "…"
		}
		paddedProject := fmt.Sprintf("%-12s", projectName)
		projectColor := secondaryColor
		if isDone || isArchived {
			projectColor = mutedColor
		}
		styledProject = lipgloss.NewStyle().Foreground(projectColor).Render(paddedProject) + " "
	}

	// Fixed-width columns before the prompt: "  " (2) + stateIcon (1) + " " (1) + unread (1) + " " (1)
	// project badge (13) is added on top when in "All" view.
	// We also reserve 9 chars on the right for the timestamp (8 chars padded + 1 space).
	const agoWidth = 9
	const draftSuffix = " draft"        // 6 chars when present
	leftFixedWidth := 2 + 1 + 1 + 1 + 1 // 6
	if isAllView {
		leftFixedWidth += projectBadgeWidth
	}
	draftExtra := 0
	if s.Draft != "" {
		draftExtra = len(draftSuffix)
	}
	maxPromptWidth := m.sessionPaneWidth() - leftFixedWidth - branchExtra - agoWidth - draftExtra
	if maxPromptWidth < 10 {
		maxPromptWidth = 10
	}

	prompt := truncateStr(sessionTitle(*s), maxPromptWidth)
	switch {
	case sessionTitleIsFallback(*s):
		prompt = lipgloss.NewStyle().Foreground(dimColor).Render(prompt)
	case isArchived:
		prompt = lipgloss.NewStyle().Foreground(mutedColor).Render(prompt)
	case isDone:
		prompt = lipgloss.NewStyle().Foreground(successColor).Render(prompt)
	case s.FollowUp:
		prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("#D97706")).Bold(true).Render(prompt)
	case s.Unread():
		prompt = lipgloss.NewStyle().Bold(true).Render(prompt)
	default:
		prompt = lipgloss.NewStyle().Foreground(dimColor).Render(prompt)
	}

	// Append lowercase red "draft" label right after the title.
	if s.Draft != "" {
		prompt += lipgloss.NewStyle().Foreground(dangerColor).Render(draftSuffix)
	}

	styledAgo := lipgloss.NewStyle().Foreground(dimColor).Render(ago)

	// Build the left portion of the line (everything except the timestamp).
	// styledProject already includes its trailing space when non-empty.
	left := fmt.Sprintf("  %s%s %s%s %s",
		styledProject,
		stateIcon,
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

	// Hard-truncate to pane width so overlong rows are clipped rather than
	// word-wrapped when an overlay applies a Width/MaxWidth style.
	line = lipgloss.NewStyle().MaxWidth(m.sessionPaneWidth()).Render(line)

	if selected {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		return prefix + line[2:]
	}
	return line
}

func (m *InboxModel) viewportHeight() int {
	reserved := 6 // header (1) + blank (1) + filter bar (1) + blank (1) + help bar (~1) + padding (1)
	if m.err != nil {
		reserved += 2
	}
	if m.showTwoPanes() {
		reserved += paneBorderInset // top/bottom border lines around the pane
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
	helpLine("tab / ← →", "switch panes")
	helpLine("n", "new branch (in sidebar)")
	helpLine("m", "merge branch (sidebar, no active chat) / focus chat + message mode (sidebar, chat shown)")
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
