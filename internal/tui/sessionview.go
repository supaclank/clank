package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/atotto/clipboard"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/daemon"
)

// sessionEventMsg wraps a daemon event delivered to the TUI.
type sessionEventMsg struct {
	event agent.Event
}

// sessionEventsErrMsg is sent when the SSE subscription fails.
type sessionEventsErrMsg struct {
	err error
}

// sessionSendResultMsg is the result of sending a follow-up message.
type sessionSendResultMsg struct {
	err error
}

// sessionFollowUpResultMsg is the result of toggling follow-up on a session.
type sessionFollowUpResultMsg struct {
	followUp bool
	err      error
}

// sessionVisibilityResultMsg is the result of setting visibility on a session.
type sessionVisibilityResultMsg struct {
	err error
}

// sessionAbortResultMsg is the result of aborting a running session.
type sessionAbortResultMsg struct {
	err error
}

// clearCtrlCHintMsg is sent after a delay to clear the "press ctrl+c again" hint.
type clearCtrlCHintMsg struct{}

// sessionInfoMsg delivers a refreshed SessionInfo to the model.
type sessionInfoMsg struct {
	info *agent.SessionInfo
}

// agentsResultMsg carries the result of fetching available agents.
type agentsResultMsg struct {
	agents []agent.AgentInfo
}

// modelsResultMsg carries the result of fetching available models.
type modelsResultMsg struct {
	models []agent.ModelInfo
}

// sessionMessagesMsg delivers the full message history to the model.
type sessionMessagesMsg struct {
	messages []agent.MessageData
}

// pendingPermissionMsg delivers restored pending permission prompts.
type pendingPermissionMsg struct {
	perms []agent.PermissionData
}

// backToInboxMsg signals navigation back to the inbox.
type backToInboxMsg struct{}

// wordBackwardBinding matches the textarea's WordBackward keys (alt+left, alt+b).
// Used to intercept the key before it reaches the textarea in cases where the
// upstream wordLeft() implementation would infinite-loop.
//
// https://github.com/charmbracelet/bubbletea/issues/1652
var wordBackwardBinding = key.NewBinding(key.WithKeys("alt+left", "alt+b"))

// wordLeftWouldHang reports whether forwarding a WordBackward key to the
// textarea would trigger the upstream infinite loop in textarea.wordLeft().
//
// The bug: wordLeft() contains an unconditional for{} loop that calls
// characterLeft(true) and breaks only when it finds a non-space rune under
// the cursor. At position (0,0) characterLeft is a no-op and the break
// condition can never be satisfied, so the loop spins forever.
//
// https://github.com/charmbracelet/bubbletea/issues/1652
func wordLeftWouldHang(ta textarea.Model) bool {
	return ta.Line() == 0 && ta.Column() == 0
}

// SessionViewModel shows a single agent session with streaming output.
// It also handles the "composing" mode where no session exists yet —
// the user types their first prompt and the session is created on send.
type SessionViewModel struct {
	client    *daemon.Client
	sessionID string
	info      *agent.SessionInfo

	// Display state.
	entries      []displayEntry // rendered message/tool entries
	cursor       int            // index into entries for the selected message
	cursorMoved  bool           // true when cursor changed since last render
	scrollOffset int            // first visible display line
	follow       bool           // auto-follow tail when true (default when busy)
	verbose      bool           // when true, the selected entry's tool calls show detail

	// Entry-to-line mapping (rebuilt by buildContentLines).
	entryStartLine []int // entryStartLine[i] = first display line for entries[i]
	entryEndLine   []int // entryEndLine[i] = last display line (exclusive) for entries[i]

	// Input state.
	inputActive bool
	input       textarea.Model

	// Permission state.
	pendingPerms []agent.PermissionData

	// SSE event channel (stored so we can re-schedule waitForEvent).
	eventsCh <-chan agent.Event

	// History deduplication. seenParts tracks part IDs loaded from history
	// so that overlapping SSE events can be skipped. historyLoaded is set
	// after the history response is processed.
	seenParts     map[string]bool
	historyLoaded bool

	// Spinner for busy/running state animation.
	spinner spinner.Model

	// Layout.
	width  int
	height int
	err    error

	// Confirm dialog state.
	showConfirm bool
	confirm     confirmDialogModel

	// Action menu state (e.g. revert on user messages).
	showMenu           bool
	menu               actionMenuModel
	menuMessageID      string // message ID the menu action targets
	menuNextMessageID  string // next message ID after the target (for fork; empty = last message)
	menuMessageContent string // message content for prompt prefill after revert

	// Model picker modal state.
	showModelPicker bool
	modelPicker     modelPickerModel

	// Help overlay state.
	showHelp bool

	// Mouse text selection state.
	selection        textSelection
	cachedContent    []string // content lines from last View(), used for selection extraction
	cachedHeaderRows int      // number of screen rows before the content area

	// standalone is true when this model is run directly (not inside InboxModel).
	// When true, 'q' quits the program instead of navigating back to inbox.
	standalone bool

	cancelEvents context.CancelFunc

	// Abort state — tracks in-flight cancellation so noisy backend events
	// (status transitions, MessageAbortedError) can be suppressed.
	aborting      bool
	abortEntryIdx int // index into entries for the "Cancelling..." line

	// Double-tap ctrl+c to quit. First press records the time; second
	// press within 1 second actually quits.
	lastCtrlC time.Time

	// Copy feedback: timestamp of the last keyboard-triggered copy so
	// renderEntry can briefly show "✓ copied" instead of "[copy]".
	copiedAt     time.Time
	copiedCursor int // entry index that was copied (flash only applies to this entry)

	// Copy button hit region (screen coordinates), set during View().
	copyBtnRow    int // screen row of the [copy] label (-1 = not visible)
	copyBtnColMin int // first column of the label (inclusive)
	copyBtnColMax int // last column of the label (exclusive)

	// Composing mode — no daemon session yet. The user is writing their
	// first prompt. After sending, this transitions to the normal session view.
	composing      bool
	backend        agent.BackendType
	projectDir     string
	worktreeBranch string // optional worktree branch to create the session on

	// Agent selection — populated eagerly when compose view loads.
	// For existing sessions opened from inbox, agents are fetched on Init.
	agents        []agent.AgentInfo
	selectedAgent int // index into agents slice

	// Model selection — populated eagerly when compose view loads.
	// The user cycles models with Shift+Tab.
	models        []agent.ModelInfo
	selectedModel int // index into models slice; -1 = use default
	// lastModelID/lastProviderID track the model from the latest assistant
	// message, used to display the active model in the header.
	lastModelID    string
	lastProviderID string

	// submitting guards against duplicate sends. Set true when a session
	// creation or message send is in flight; cleared on result.
	submitting bool

	// voice points to the InboxModel's voice state for rendering
	// (header badge, help bar). Nil when running standalone without
	// an InboxModel parent.
	voice *voiceState
}

// displayEntry is a rendered item in the session transcript.
type displayEntry struct {
	kind          entryKind
	partID        string // Part ID for tracking updates (text deltas, tool status)
	messageID     string // Backend message ID (for revert targeting)
	nextMessageID string // ID of the next message in conversation order (for fork targeting; empty = last message)
	content       string // pre-rendered line(s), may contain ANSI
	agent         string // agent mode used for this entry (only set for entryUser)

	// toolPart stores the original Part data for entryTool entries so the
	// tool line (including its spinner) can be rendered live during View()
	// instead of being baked into content once.
	toolPart *agent.Part

	// streaming is true while the entry is still receiving streamed deltas.
	// Only the actively-streaming entry uses plain wrapText; completed
	// entries render full markdown even when the session is busy.
	streaming bool

	// Markdown render cache. Populated lazily in renderEntry() to avoid
	// re-running glamour on every View() cycle. Invalidated when content
	// changes (e.g. streaming deltas) by clearing renderedMD.
	renderedMD    string // cached glamour output (empty = not yet rendered)
	renderedWidth int    // terminal width the cache was computed at

	// Tool render caches. The summary line is cached for tools in terminal
	// status (completed/error) so the spinner can still animate for running
	// tools. Verbose lines are cached separately.
	// Both are invalidated when toolPart changes or terminal width changes.
	toolLine     string      // cached summary line (empty = not yet rendered or running)
	verboseLines []string    // cached verbose output lines (nil = not yet rendered)
	verboseWidth int         // width the verbose cache was computed at
	expand       expandState // click-toggled detail expansion for individual tool entries
}

// expandState controls per-entry detail visibility relative to the owning
// navigable entry's expanded state.
type expandState int8

const (
	expandDefault   expandState = 0  // follow ownerExpanded
	expandForceShow expandState = 1  // always show detail (clicked open outside verbose)
	expandForceHide expandState = -1 // always hide detail (clicked closed inside verbose)
)

type entryKind int

const (
	entryUser   entryKind = iota // User message
	entryText                    // Agent text
	entryTool                    // Tool call line
	entryThink                   // Thinking block
	entryError                   // Error message
	entryPerm                    // Permission prompt
	entryStatus                  // Status change
)

// inputReservedLines is the number of terminal lines reserved for the
// textarea + help when the input prompt is visible. Used by contentHeight
// and the scroll-offset adjustments that keep the viewport anchored when
// the input is toggled.
const inputReservedLines = 6

// NewSessionViewModel creates a session detail TUI for an existing session.
func NewSessionViewModel(client *daemon.Client, sessionID string) *SessionViewModel {
	ta := newPromptTextarea("Type a follow-up message...", 3)
	sp := spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(successColor)),
	)
	return &SessionViewModel{
		client:    client,
		sessionID: sessionID,
		follow:    true,
		input:     ta,
		spinner:   sp,
	}
}

// SetStandalone marks this view as running standalone (not inside InboxModel).
// When standalone, 'q' quits the program instead of navigating back to inbox.
func (m *SessionViewModel) SetStandalone(v bool) {
	m.standalone = v
}

// SetWorktreeBranch sets the git worktree branch for the session (used in composing mode).
func (m *SessionViewModel) SetWorktreeBranch(branch string) {
	m.worktreeBranch = branch
}

// DraftText returns the current unsent text in the input textarea.
func (m *SessionViewModel) DraftText() string {
	return m.input.Value()
}

// RestoreDraft sets the textarea content to the given draft text and
// activates the input so the user can continue typing immediately.
func (m *SessionViewModel) RestoreDraft(text string) {
	m.input.SetValue(text)
	m.inputActive = true
	if !m.follow {
		m.scrollOffset += inputReservedLines
	}
	m.input.Focus()
}

// SetEventChannel provides a pre-connected SSE event channel and cancel func.
// When set, Init() skips subscribing and immediately starts reading events.
// This avoids the race where CreateSession emits events before the TUI subscribes.
func (m *SessionViewModel) SetEventChannel(ch <-chan agent.Event, cancel context.CancelFunc) {
	m.eventsCh = ch
	m.cancelEvents = cancel
}

func (m *SessionViewModel) Init() tea.Cmd {
	// In composing mode, no session exists yet — nothing to subscribe to.
	if m.composing {
		return tea.Batch(m.input.Focus(), m.fetchAgents(), m.fetchModels())
	}
	cmds := []tea.Cmd{m.fetchSessionInfo(), m.fetchSessionMessages(), m.fetchPendingPermission(), m.spinner.Tick}
	if m.eventsCh != nil {
		// Already connected — start reading immediately.
		cmds = append(cmds, waitForEvent(m.eventsCh, m.sessionID))
	} else {
		cmds = append(cmds, m.subscribeEvents())
	}
	return tea.Batch(cmds...)
}

// fetchSessionInfo loads the current session info from the daemon.
func (m *SessionViewModel) fetchSessionInfo() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		info, err := m.client.GetSession(ctx, m.sessionID)
		if err != nil {
			return sessionEventsErrMsg{err: err}
		}
		return sessionInfoMsg{info: info}
	}
}

// fetchSessionMessages loads the full message history from the daemon.
func (m *SessionViewModel) fetchSessionMessages() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		messages, err := m.client.GetSessionMessages(ctx, m.sessionID)
		if err != nil {
			return sessionEventsErrMsg{err: err}
		}
		return sessionMessagesMsg{messages: messages}
	}
}

// fetchPendingPermission checks if the daemon has pending permissions for
// this session that were emitted while the TUI was disconnected.
func (m *SessionViewModel) fetchPendingPermission() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		perms, err := m.client.GetPendingPermissions(ctx, m.sessionID)
		if err != nil {
			// Non-critical; the live SSE stream will deliver new prompts.
			return nil
		}
		return pendingPermissionMsg{perms: perms}
	}
}

// fetchAgents loads the available agents for the current backend/project.
// Fired eagerly on compose init; the result arrives before the user finishes typing.
func (m *SessionViewModel) fetchAgents() tea.Cmd {
	client := m.client
	backend := m.backend
	projectDir := m.projectDir
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		agents, err := client.ListAgents(ctx, backend, projectDir)
		if err != nil {
			// Non-fatal: degrade gracefully with no agent selector.
			return agentsResultMsg{}
		}
		return agentsResultMsg{agents: agents}
	}
}

// fetchModels loads available models for the current backend/project.
// Fired eagerly on compose init alongside fetchAgents.
func (m *SessionViewModel) fetchModels() tea.Cmd {
	client := m.client
	backend := m.backend
	projectDir := m.projectDir
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		models, err := client.ListModels(ctx, backend, projectDir)
		if err != nil {
			// Non-fatal: degrade gracefully with no model selector.
			return modelsResultMsg{}
		}
		return modelsResultMsg{models: models}
	}
}

// subscribeEvents starts the SSE subscription and delivers events as messages.
func (m *SessionViewModel) subscribeEvents() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		events, err := m.client.SubscribeEvents(ctx)
		if err != nil {
			cancel()
			return sessionEventsErrMsg{err: err}
		}
		return sseSetupMsg{events: events, cancel: cancel}
	}
}

// sseSetupMsg carries the SSE channel to the model so it can be stored.
type sseSetupMsg struct {
	events <-chan agent.Event
	cancel context.CancelFunc
}

// sseClosedMsg signals the SSE stream was closed intentionally.
type sseClosedMsg struct{}

// waitForEvent returns a command that waits for the next event from the channel.
func waitForEvent(events <-chan agent.Event, sessionID string) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-events
		if !ok {
			// Channel closed — intentional shutdown, not an error.
			return sseClosedMsg{}
		}
		// Filter to our session (or session lifecycle events).
		if evt.SessionID == sessionID || evt.Type == agent.EventSessionCreate || evt.Type == agent.EventSessionDelete {
			return sessionEventMsg{event: evt}
		}
		// Not for us — skip and wait for the next one.
		// Return a "skip" that re-subscribes.
		return sseSkipMsg{events: events, sessionID: sessionID}
	}
}

// sseSkipMsg tells Update to re-wait for the next event.
type sseSkipMsg struct {
	events    <-chan agent.Event
	sessionID string
}

func (m *SessionViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Always keep the spinner ticking, regardless of modal/composing state.
	// The spinner's tick chain is self-sustaining: each Update schedules
	// the next tick. Swallowing a single TickMsg permanently kills it.
	if tickMsg, ok := msg.(spinner.TickMsg); ok {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(tickMsg)
		return m, cmd
	}

	// Composing mode has its own update path.
	if m.composing {
		return m.updateCompose(msg)
	}

	// Confirm dialog takes priority when open.
	if m.showConfirm {
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

	// Action menu takes priority when open.
	if m.showMenu {
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

	// Model picker takes priority when open.
	if m.showModelPicker {
		switch msg := msg.(type) {
		case modelPickerResultMsg:
			m.showModelPicker = false
			m.selectedModel = msg.selectedModel
			go m.persistModelPreference()
			return m, nil
		case modelPickerCancelMsg:
			m.showModelPicker = false
			return m, nil
		default:
			var cmd tea.Cmd
			m.modelPicker, cmd = m.modelPicker.Update(msg)
			return m, cmd
		}
	}

	// Help overlay takes priority when open — dismiss on any key.
	if m.showHelp {
		if _, ok := msg.(tea.KeyPressMsg); ok {
			m.showHelp = false
			return m, nil
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// SetWidth takes the textarea's own outer width. Since we render the
		// border externally, subtract the external frame.
		m.input.SetWidth(m.width - promptInputBorderSize)
		return m, nil

	case sessionInfoMsg:
		m.info = msg.info
		// Don't seed entries from session info — wait for the full message
		// history (sessionMessagesMsg) to avoid a flash of the bare prompt
		// before the complete conversation renders.

		// Fetch agents if we don't have them yet (existing sessions opened from inbox).
		if len(m.agents) == 0 && m.info.Backend == agent.BackendOpenCode && m.info.ProjectDir != "" {
			m.backend = m.info.Backend
			m.projectDir = m.info.ProjectDir
			// Restore the selected agent from session info.
			if m.info.Agent != "" {
				// Will be matched against the fetched list in agentsResultMsg.
			}
			return m, tea.Batch(m.fetchAgents(), m.fetchModels())
		}
		return m, nil

	case nativeCLIReturnMsg:
		// User returned from native CLI — re-fetch session state to pick
		// up messages and status changes made in the external TUI.
		if msg.err != nil {
			m.err = msg.err
		}
		return m, tea.Batch(m.fetchSessionInfo(), m.fetchSessionMessages())

	case agentsResultMsg:
		m.agents = msg.agents
		// Try to match the session's current agent.
		selectedName := "build"
		if m.info != nil && m.info.Agent != "" {
			selectedName = m.info.Agent
		}
		for i, a := range m.agents {
			if a.Name == selectedName {
				m.selectedAgent = i
				break
			}
		}
		return m, nil

	case modelsResultMsg:
		m.models = msg.models
		m.selectedModel = -1 // default: no override

		// Try to restore the user's preferred model from preferences.
		prefs, _ := config.LoadPreferences()
		if prefs.Model != nil {
			for i, model := range m.models {
				if model.ID == prefs.Model.ModelID && model.ProviderID == prefs.Model.ProviderID {
					m.selectedModel = i
					break
				}
			}
		}
		return m, nil

	case sessionMessagesMsg:
		m.handleSessionMessages(msg.messages)
		return m, nil

	case pendingPermissionMsg:
		// Restore pending permissions that were emitted while the TUI was
		// disconnected. Skip any that are already in our queue (from the live
		// SSE stream) by checking request IDs.
		seen := make(map[string]bool, len(m.pendingPerms))
		for _, p := range m.pendingPerms {
			seen[p.RequestID] = true
		}
		for _, p := range msg.perms {
			if seen[p.RequestID] {
				continue
			}
			m.pendingPerms = append(m.pendingPerms, p)
			m.entries = append(m.entries, displayEntry{
				kind:    entryPerm,
				content: fmt.Sprintf("Allow %s: %s? [y/n]", p.Tool, p.Description),
			})
		}
		if len(msg.perms) > 0 && m.follow {
			m.scrollToBottom()
		}
		return m, nil

	case sseSetupMsg:
		m.cancelEvents = msg.cancel
		m.eventsCh = msg.events
		return m, waitForEvent(m.eventsCh, m.sessionID)

	case sseSkipMsg:
		return m, waitForEvent(msg.events, msg.sessionID)

	case sseClosedMsg:
		// SSE stream closed intentionally (e.g. user quit). No-op.
		m.eventsCh = nil
		return m, nil

	case sessionEventMsg:
		m.handleEvent(msg.event)
		// Re-schedule to wait for the next event.
		if m.eventsCh != nil {
			return m, waitForEvent(m.eventsCh, m.sessionID)
		}
		return m, nil

	case sessionSendResultMsg:
		m.submitting = false
		if msg.err != nil {
			m.err = msg.err
		}
		return m, nil

	case sessionFollowUpResultMsg:
		if msg.err != nil {
			m.err = msg.err
		} else if m.info != nil {
			m.info.FollowUp = msg.followUp
		}
		return m, nil

	case sessionVisibilityResultMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// After successfully marking done/archived, go back to inbox.
		if m.cancelEvents != nil {
			m.cancelEvents()
		}
		return m, func() tea.Msg { return backToInboxMsg{} }

	case revertResultMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Store the revert state so handleSessionMessages can filter messages.
		m.info.RevertMessageID = msg.messageID
		// Reload the message history and prefill the prompt with the
		// reverted user message so the user can edit and resend.
		m.err = nil
		m.input.SetValue(msg.prompt)
		m.inputActive = true
		m.input.Focus()
		m.follow = true
		return m, m.fetchSessionMessages()

	case forkResultMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Tell the inbox to navigate to the new forked session.
		return m, func() tea.Msg { return openForkedSessionMsg{sessionID: msg.sessionID} }

	case sessionEventsErrMsg:
		m.err = msg.err
		return m, nil

	case sessionAbortResultMsg:
		if msg.err != nil {
			m.err = fmt.Errorf("abort: %w", msg.err)
			if m.aborting && m.abortEntryIdx < len(m.entries) {
				m.entries[m.abortEntryIdx].content = "Cancel failed"
			}
			m.aborting = false
		}
		return m, nil

	case clearCtrlCHintMsg:
		m.lastCtrlC = time.Time{}
		return m, nil

	case tea.MouseWheelMsg:
		// Mouse scroll is line-by-line, independent of cursor selection.
		m.selection.Clear()
		switch msg.Button {
		case tea.MouseWheelUp:
			m.follow = false
			if m.scrollOffset > 0 {
				m.scrollOffset -= 3
				if m.scrollOffset < 0 {
					m.scrollOffset = 0
				}
			}
		case tea.MouseWheelDown:
			m.scrollOffset += 3
			if m.clampScroll() {
				m.follow = true
			}
		}
		return m, nil

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			m.selection.Start(msg.X, msg.Y)
		}
		return m, nil

	case tea.MouseMotionMsg:
		// MouseMotionMsg with button held (cell-motion mode) — update drag.
		if msg.Button == tea.MouseLeft {
			m.selection.Update(msg.X, msg.Y)
		}
		return m, nil

	case tea.MouseReleaseMsg:
		if m.selection.HasSelection() {
			m.selection.Finish(m.cachedContent, m.cachedHeaderRows, m.scrollOffset)
		} else {
			m.selection.Clear()
			// Check if the click landed on the [copy] button.
			if m.copyBtnRow >= 0 && msg.Y == m.copyBtnRow && msg.X >= m.copyBtnColMin && msg.X < m.copyBtnColMax {
				if m.cursor >= 0 && m.cursor < len(m.entries) {
					e := m.entries[m.cursor]
					if e.content != "" {
						_ = clipboard.WriteAll(e.content)
						m.copiedAt = time.Now()
						m.copiedCursor = m.cursor
					}
				}
			} else if idx := m.entryAtScreenY(msg.Y); idx >= 0 && idx < len(m.entries) {
				entry := &m.entries[idx]
				if entry.kind == entryTool {
					// Click on a tool entry toggles its detail.
					// Determine whether the entry is currently visible, accounting for
					// always-expanded tools (edit, todowrite) and owner expansion state.
					alwaysExpand := entry.toolPart != nil &&
						(strings.EqualFold(entry.toolPart.Tool, "todowrite") ||
							strings.EqualFold(entry.toolPart.Tool, "edit"))
					ownerExpanded := false
					for j := idx - 1; j >= 0; j-- {
						if isNavigable(m.entries[j].kind) {
							ownerExpanded = m.verbose && j == m.cursor
							break
						}
					}
					defaultVisible := alwaysExpand || ownerExpanded
					if defaultVisible {
						// Currently visible by default: toggle between default (visible) and forceHide.
						if entry.expand == expandForceHide {
							entry.expand = expandDefault
						} else {
							entry.expand = expandForceHide
						}
					} else {
						// Currently hidden by default: toggle between default (hidden) and forceShow.
						if entry.expand == expandForceShow {
							entry.expand = expandDefault
						} else {
							entry.expand = expandForceShow
						}
					}
					entry.verboseLines = nil
				} else if isNavigable(entry.kind) && idx != m.cursor {
					// Click-to-select: move cursor but don't scroll (no m.cursorMoved)
					// to avoid layout shift — the clicked entry is already visible.
					m.cursor = idx
					// Disable follow mode if selecting an earlier entry; clicking the
					// last navigable entry keeps follow active.
					if idx != m.lastNavigableEntry() {
						m.follow = false
					}
				}
			}
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	// Update textarea if active.
	if m.inputActive {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *SessionViewModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	msg = normalizeKeyCase(msg)

	// Permission prompt takes priority — respond to the front of the queue.
	if len(m.pendingPerms) > 0 && !m.inputActive {
		switch msg.String() {
		case "y":
			perm := m.pendingPerms[0]
			m.pendingPerms = m.pendingPerms[1:]
			m.entries = append(m.entries, displayEntry{
				kind:    entryStatus,
				content: "Permission granted: " + perm.Tool,
			})
			return m, m.replyPermission(perm.RequestID, true)
		case "n":
			perm := m.pendingPerms[0]
			m.pendingPerms = m.pendingPerms[1:]
			m.entries = append(m.entries, displayEntry{
				kind:    entryStatus,
				content: "Permission denied: " + perm.Tool,
			})
			return m, m.replyPermission(perm.RequestID, false)
		}
	}

	// Input mode.
	if m.inputActive {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
			if m.info != nil && (m.info.Status == agent.StatusBusy || m.info.Status == agent.StatusStarting) {
				return m, m.startAbort()
			}
			return m.handleCtrlCQuit()
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.inputActive = false
			m.input.Blur()
			if !m.follow {
				// Shift content back down: the viewport just grew by
				// inputReservedLines, so undo the offset bump from opening.
				m.scrollOffset -= inputReservedLines
				if m.scrollOffset < 0 {
					m.scrollOffset = 0
				}
			}
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
			// Cycle through agents.
			if len(m.agents) > 1 {
				m.selectedAgent = (m.selectedAgent + 1) % len(m.agents)
			}
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))):
			// Open model picker modal.
			if len(m.models) > 0 {
				m.showModelPicker = true
				m.modelPicker = newModelPicker(m.models, m.selectedModel, m.backend)
			}
			return m, m.modelPicker.Init()
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			// Send the message. Shift+enter inserts newline (handled by textarea).
			if m.submitting {
				return m, nil // Already in flight — ignore duplicate Enter.
			}
			text := strings.TrimSpace(m.input.Value())
			if text != "" {
				agentName := ""
				if len(m.agents) > 0 {
					agentName = m.agents[m.selectedAgent].Name
				}
				// Clear revert state — the user is sending a new message,
				// so any previously reverted messages should reappear.
				if m.info != nil {
					m.info.RevertMessageID = ""
				}
				m.entries = append(m.entries, displayEntry{
					kind:    entryUser,
					content: text,
					agent:   agentName,
				})
				m.input.Reset()
				m.inputActive = false
				m.input.Blur()
				m.follow = true
				m.submitting = true
				return m, m.sendMessage(text)
			}
			return m, nil
		case key.Matches(msg, wordBackwardBinding):
			// Workaround: upstream bubbles textarea.wordLeft() has an
			// unconditional for{} loop that never terminates when the cursor
			// is at (0,0) — the empty-input case. Intercept and no-op here
			// to prevent an infinite loop that freezes the entire UI.
			// See: https://github.com/charmbracelet/bubbles/issues/XXX
			if wordLeftWouldHang(m.input) {
				return m, nil
			}
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// Normal mode.
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		if m.info != nil && (m.info.Status == agent.StatusBusy || m.info.Status == agent.StatusStarting) {
			return m, m.startAbort()
		}
		return m.handleCtrlCQuit()
	case key.Matches(msg, key.NewBinding(key.WithKeys("q", "esc"))):
		if m.cancelEvents != nil {
			m.cancelEvents()
		}
		if m.standalone {
			return m, tea.Quit
		}
		return m, func() tea.Msg { return backToInboxMsg{} }
	case key.Matches(msg, key.NewBinding(key.WithKeys("m"))):
		m.inputActive = true
		if !m.follow {
			// Shift content up so the bottom of the visible window stays
			// anchored when the input prompt appears and shrinks the viewport.
			m.scrollOffset += inputReservedLines
		}
		return m, m.input.Focus()
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
		m.follow = false
		if idx := m.prevNavigableEntry(m.cursor); idx >= 0 {
			m.cursor = idx
			m.cursorMoved = true
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
		if idx := m.nextNavigableEntry(m.cursor); idx >= 0 {
			m.follow = false
			m.cursor = idx
			m.cursorMoved = true
		} else {
			// Already at last navigable entry — enable follow.
			m.follow = true
			m.scrollToBottom()
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+up"))):
		m.follow = false
		if idx := m.prevUserEntry(m.cursor); idx >= 0 {
			m.cursor = idx
			m.cursorMoved = true
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+down"))):
		if idx := m.nextUserEntry(m.cursor); idx >= 0 {
			m.follow = false
			m.cursor = idx
			m.cursorMoved = true
		} else {
			// No more user messages below — jump to last navigable entry and follow.
			m.follow = true
			m.cursor = m.lastNavigableEntry()
			m.cursorMoved = true
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("pgup", "ctrl+u"))):
		m.follow = false
		// Jump cursor by ~half viewport worth of navigable entries.
		jumps := m.contentHeight() / 4 // approximate: entries are ~2-4 lines each
		if jumps < 1 {
			jumps = 1
		}
		for i := 0; i < jumps; i++ {
			if idx := m.prevNavigableEntry(m.cursor); idx >= 0 {
				m.cursor = idx
				m.cursorMoved = true
			} else {
				break
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown", "ctrl+d"))):
		jumps := m.contentHeight() / 4
		if jumps < 1 {
			jumps = 1
		}
		for i := 0; i < jumps; i++ {
			if idx := m.nextNavigableEntry(m.cursor); idx >= 0 {
				m.cursor = idx
				m.cursorMoved = true
			} else {
				break
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("G", "end"))):
		m.follow = true
		m.cursor = m.lastNavigableEntry()
		m.cursorMoved = true
	case key.Matches(msg, key.NewBinding(key.WithKeys("g", "home"))):
		m.follow = false
		m.cursor = m.firstNavigableEntry()
		m.cursorMoved = true
	case key.Matches(msg, key.NewBinding(key.WithKeys("a"))):
		// TODO: approve session
	case key.Matches(msg, key.NewBinding(key.WithKeys(":"))):
		// Open action menu for the selected entry.
		if m.cursor >= 0 && m.cursor < len(m.entries) {
			entry := m.entries[m.cursor]
			if entry.messageID != "" {
				m.menuMessageID = entry.messageID
				m.menuNextMessageID = entry.nextMessageID
				m.menuMessageContent = entry.content
				m.showMenu = true
				var items []actionMenuItem
				if entry.kind == entryUser {
					items = append(items, actionMenuItem{label: "Revert to this message", key: "r", action: "revert"})
				}
				items = append(items, actionMenuItem{label: "Fork from here", key: "f", action: "fork"})
				m.menu = newActionMenu("Actions", items)
				return m, nil
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("v"))):
		m.verbose = !m.verbose
		for i := range m.entries {
			if m.entries[i].expand != expandDefault {
				m.entries[i].expand = expandDefault
				m.entries[i].verboseLines = nil
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("c"))):
		if m.cursor >= 0 && m.cursor < len(m.entries) {
			e := m.entries[m.cursor]
			if e.content != "" {
				_ = clipboard.WriteAll(e.content)
				m.copiedAt = time.Now()
				m.copiedCursor = m.cursor
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("f"))):
		return m, m.toggleFollowUp()
	case key.Matches(msg, key.NewBinding(key.WithKeys("d"))):
		title := "this session"
		if m.info != nil {
			if m.info.Title != "" {
				title = truncateStr(m.info.Title, 40)
			} else if m.info.Prompt != "" {
				title = truncateStr(m.info.Prompt, 40)
			}
		}
		m.showConfirm = true
		m.confirm = newConfirmDialog(
			"Mark as Done",
			fmt.Sprintf("Mark '%s' as done?\nIt will be hidden from the inbox.", title),
			"done",
		)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("x"))):
		title := "this session"
		if m.info != nil {
			if m.info.Title != "" {
				title = truncateStr(m.info.Title, 40)
			} else if m.info.Prompt != "" {
				title = truncateStr(m.info.Prompt, 40)
			}
		}
		m.showConfirm = true
		m.confirm = newConfirmDialog(
			"Archive Session",
			fmt.Sprintf("Archive '%s'?\nIt will be hidden from the inbox.", title),
			"archive",
		)
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("o"))):
		return m, openNativeCLI(m.info)
	case key.Matches(msg, key.NewBinding(key.WithKeys("?"))):
		m.showHelp = true
		return m, nil
	}

	return m, nil
}

func (m *SessionViewModel) handleEvent(evt agent.Event) {
	switch evt.Type {
	case agent.EventStatusChange:
		if data, ok := evt.Data.(agent.StatusChangeData); ok {
			m.handleStatusChange(data)
		}

	case agent.EventMessage:
		if data, ok := evt.Data.(agent.MessageData); ok {
			m.handleMessage(data)
		}

	case agent.EventPartUpdate:
		if data, ok := evt.Data.(agent.PartUpdateData); ok {
			m.handlePartUpdate(data)
		}

	case agent.EventPermission:
		if data, ok := evt.Data.(agent.PermissionData); ok {
			m.pendingPerms = append(m.pendingPerms, data)
			m.entries = append(m.entries, displayEntry{
				kind:    entryPerm,
				content: fmt.Sprintf("Allow %s: %s? [y/n]", data.Tool, data.Description),
			})
			if m.follow {
				m.scrollToBottom()
			}
		}

	case agent.EventError:
		if m.aborting {
			// Suppress abort-related error noise (e.g. MessageAbortedError).
			// The "Cancelled" status entry handles the UX.
			break
		}
		if data, ok := evt.Data.(agent.ErrorData); ok {
			m.entries = append(m.entries, displayEntry{
				kind:    entryError,
				content: data.Message,
			})
		}
		if m.follow {
			m.scrollToBottom()
		}

	case agent.EventSessionDelete:
		if evt.SessionID == m.sessionID {
			m.entries = append(m.entries, displayEntry{
				kind:    entryStatus,
				content: "Session deleted",
			})
		}

	case agent.EventTitleChange:
		if data, ok := evt.Data.(agent.TitleChangeData); ok {
			if m.info != nil {
				m.info.Title = data.Title
			}
		}

	case agent.EventRevertChange:
		if data, ok := evt.Data.(agent.RevertChangeData); ok {
			if m.info != nil {
				m.info.RevertMessageID = data.MessageID
			}
		}
	}
}

func (m *SessionViewModel) handleStatusChange(data agent.StatusChangeData) {
	if m.info != nil {
		m.info.Status = data.NewStatus
	}
	// When the agent is no longer streaming, mark all entries as
	// non-streaming so they switch from plain wrapText to full
	// markdown rendering on the next View() cycle.
	if data.NewStatus != agent.StatusBusy && data.NewStatus != agent.StatusStarting {
		for i := range m.entries {
			m.entries[i].streaming = false
		}
	}
	if m.aborting {
		// Suppress noisy intermediate status entries during cancellation.
		// When the agent settles, update the existing "Cancelling..." entry.
		if data.NewStatus != agent.StatusBusy && data.NewStatus != agent.StatusStarting {
			if m.abortEntryIdx < len(m.entries) {
				m.entries[m.abortEntryIdx].content = "Cancelled"
			}
			m.aborting = false
		}
		if m.follow {
			m.scrollToBottom()
		}
		return
	}
	m.entries = append(m.entries, displayEntry{
		kind:    entryStatus,
		content: fmt.Sprintf("%s -> %s", data.OldStatus, data.NewStatus),
	})
	// Auto-follow when agent becomes busy.
	if data.NewStatus == agent.StatusBusy {
		m.follow = true
	}
	if m.follow {
		m.scrollToBottom()
	}
}

func (m *SessionViewModel) handleMessage(data agent.MessageData) {
	if data.Role == "user" {
		// Backfill messageID on the most recent user entry that lacks one.
		// Inline user entries (added when the user sends a message) are
		// created before the server assigns an ID. The SSE event carries
		// the server-assigned ID so we can enable actions (e.g. revert).
		if data.ID != "" {
			for i := len(m.entries) - 1; i >= 0; i-- {
				if m.entries[i].kind == entryUser && m.entries[i].messageID == "" {
					m.entries[i].messageID = data.ID
					break
				}
			}
		}
		return
	}

	// Track the model used by the latest assistant message for header display.
	if data.ModelID != "" {
		m.lastModelID = data.ModelID
		m.lastProviderID = data.ProviderID
	}
	// After history is loaded, skip SSE message events entirely —
	// history already contains the complete messages, and SSE only delivers
	// redundant shells. New parts arrive via EventPartUpdate.
	if m.historyLoaded {
		return
	}
	// Skip empty assistant messages (no content and no parts).
	if data.Content == "" && len(data.Parts) == 0 {
		return
	}
	if data.Content != "" {
		m.entries = append(m.entries, displayEntry{
			kind:    entryText,
			content: data.Content,
		})
	}
	// Also render any parts.
	for _, p := range data.Parts {
		m.addPartEntry(p)
	}
	if m.follow {
		m.scrollToBottom()
	}
}

// handleSessionMessages processes the full message history response.
// It replaces any existing entries with the complete history and builds
// the seenParts set for deduplication with subsequent SSE events.
// When a revert is active (m.info.RevertMessageID is set), messages from
// the revert target onward are excluded from the display.
func (m *SessionViewModel) handleSessionMessages(messages []agent.MessageData) {
	m.seenParts = make(map[string]bool)
	m.entries = nil

	revertID := ""
	if m.info != nil {
		revertID = m.info.RevertMessageID
	}

	// Collect ordered message IDs for building the nextMessageID mapping.
	var msgIDs []string

	for _, msg := range messages {
		// If this message is the revert target, stop — exclude it and
		// everything after it.
		if revertID != "" && msg.ID == revertID {
			break
		}

		if msg.ID != "" {
			msgIDs = append(msgIDs, msg.ID)
		}

		if msg.Role == "user" {
			// Reconstruct user message content from text parts.
			var text string
			for _, p := range msg.Parts {
				if p.Type == agent.PartText {
					text += p.Text
				}
			}
			if text == "" {
				text = msg.Content
			}
			if text != "" {
				m.entries = append(m.entries, displayEntry{
					kind:      entryUser,
					messageID: msg.ID,
					content:   text,
				})
			}
			continue
		}

		// Track model from assistant messages for header display.
		if msg.ModelID != "" {
			m.lastModelID = msg.ModelID
			m.lastProviderID = msg.ProviderID
		}

		// Assistant message: render parts.
		if msg.Content != "" && len(msg.Parts) == 0 {
			m.entries = append(m.entries, displayEntry{
				kind:      entryText,
				messageID: msg.ID,
				content:   msg.Content,
			})
		}
		for _, p := range msg.Parts {
			if p.ID != "" {
				m.seenParts[p.ID] = true
			}
			m.addPartEntry(p, msg.ID)
		}
	}

	// Build messageID → nextMessageID mapping and apply to entries.
	nextMap := make(map[string]string, len(msgIDs))
	for i, id := range msgIDs {
		if i+1 < len(msgIDs) {
			nextMap[id] = msgIDs[i+1]
		}
		// Last message: nextMap[id] remains "" (fork entire session).
	}
	for i := range m.entries {
		if m.entries[i].messageID != "" {
			m.entries[i].nextMessageID = nextMap[m.entries[i].messageID]
		}
	}

	m.historyLoaded = true
	if m.follow {
		m.scrollToBottom()
	}
}

func (m *SessionViewModel) handlePartUpdate(data agent.PartUpdateData) {
	// Skip parts already loaded from history to avoid duplicates.
	if m.seenParts[data.Part.ID] {
		return
	}
	m.upsertPartEntry(data.Part, data.IsDelta)
	if m.follow {
		m.scrollToBottom()
	}
}

// upsertPartEntry updates an existing entry with the same Part ID, or appends a new one.
// For text/thinking parts: isDelta=true appends the text chunk; isDelta=false replaces
// the entry with the authoritative full snapshot (self-healing if deltas were dropped).
// For tool parts, the entry is always replaced with the new status.
func (m *SessionViewModel) upsertPartEntry(p agent.Part, isDelta bool) {
	if p.ID != "" {
		for i := len(m.entries) - 1; i >= 0; i-- {
			e := &m.entries[i]
			if e.partID == p.ID {
				switch p.Type {
				case agent.PartToolCall, agent.PartToolResult:
					// Merge: preserve Input/Output from the existing entry
					// when the new update doesn't carry them. This handles
					// the Claude pattern where PartToolCall has Input and
					// a later PartToolResult has Output for the same ID.
					pCopy := p
					if e.toolPart != nil {
						if pCopy.Input == nil && e.toolPart.Input != nil {
							pCopy.Input = e.toolPart.Input
						}
						if pCopy.Output == "" && e.toolPart.Output != "" {
							pCopy.Output = e.toolPart.Output
						}
						if pCopy.Tool == "" && e.toolPart.Tool != "" {
							pCopy.Tool = e.toolPart.Tool
						}
					}
					e.toolPart = &pCopy
					e.toolLine = ""      // invalidate summary cache
					e.verboseLines = nil // invalidate verbose cache
				case agent.PartText, agent.PartThinking:
					if isDelta {
						e.content += p.Text
						e.streaming = true
					} else {
						e.content = p.Text
						e.streaming = false
					}
					e.renderedMD = "" // invalidate markdown cache
				}
				return
			}
		}
	}
	m.addPartEntry(p)
}

func (m *SessionViewModel) addPartEntry(p agent.Part, messageID ...string) {
	msgID := ""
	if len(messageID) > 0 {
		msgID = messageID[0]
	}
	switch p.Type {
	case agent.PartToolCall, agent.PartToolResult:
		pCopy := p
		m.entries = append(m.entries, displayEntry{
			kind:      entryTool,
			partID:    p.ID,
			messageID: msgID,
			toolPart:  &pCopy,
		})
	case agent.PartThinking:
		if p.Text != "" {
			m.entries = append(m.entries, displayEntry{
				kind:      entryThink,
				partID:    p.ID,
				messageID: msgID,
				content:   p.Text,
				streaming: m.isBusy(),
			})
		}
	case agent.PartText:
		m.entries = append(m.entries, displayEntry{
			kind:      entryText,
			partID:    p.ID,
			messageID: msgID,
			content:   p.Text,
			streaming: m.isBusy(),
		})
	}
}

func (m *SessionViewModel) renderToolLine(p agent.Part) string {
	icon := m.statusIcon(p.Status)
	label := p.Tool
	if label == "" {
		label = string(p.Type)
	}

	// Extract a concise, always-visible description from the tool input.
	// File tools show their path; Bash shows the command; search tools show
	// the pattern. Falls back to Part.Text when Input is unavailable.
	desc := m.toolSummary(p)
	if desc == "" {
		desc = p.Text
	}
	if len(desc) > 80 {
		desc = desc[:77] + "..."
	}

	if desc != "" {
		return fmt.Sprintf("[%s] %s %s", label, icon, desc)
	}
	return fmt.Sprintf("[%s] %s", label, icon)
}

// toolSummary returns a short description extracted from the tool's input
// arguments. File tools show project-relative paths; Read includes a line
// range; Bash shows the command; search tools show the pattern.
func (m *SessionViewModel) toolSummary(p agent.Part) string {
	if p.Input == nil {
		return ""
	}
	switch strings.ToLower(p.Tool) {
	case "read":
		fp, _ := p.Input["filePath"].(string)
		if fp == "" {
			return ""
		}
		fp = m.relPath(fp)
		// Build line range from offset/limit (JSON numbers arrive as float64).
		offset, _ := p.Input["offset"].(float64)
		limit, _ := p.Input["limit"].(float64)
		start := int(offset)
		if start < 1 {
			start = 1
		}
		if limit > 0 {
			return fmt.Sprintf("%s:%d-%d", fp, start, start+int(limit)-1)
		}
		if start > 1 {
			return fmt.Sprintf("%s:%d", fp, start)
		}
		return fp
	case "write", "edit":
		if fp, ok := p.Input["filePath"].(string); ok {
			return m.relPath(fp)
		}
	case "glob":
		pat, _ := p.Input["pattern"].(string)
		dir, _ := p.Input["path"].(string)
		dir = m.relPath(dir)
		if dir != "" {
			return dir + "/" + pat
		}
		return pat
	case "grep":
		pat, _ := p.Input["pattern"].(string)
		inc, _ := p.Input["include"].(string)
		if inc != "" {
			return pat + " (" + inc + ")"
		}
		return pat
	case "bash":
		if cmd, ok := p.Input["command"].(string); ok {
			return cmd
		}
	case "task":
		if desc, ok := p.Input["description"].(string); ok {
			return desc
		}
	}
	// Generic fallback: look for common keys.
	for _, k := range []string{"filePath", "path", "url", "command", "description"} {
		if v, ok := p.Input[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// relPath strips the project directory prefix from an absolute path,
// returning a project-relative path. Returns the input unchanged if it
// doesn't start with the project dir or projectDir is unset.
func (m *SessionViewModel) relPath(fp string) string {
	if m.projectDir == "" || fp == "" {
		return fp
	}
	prefix := m.projectDir
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if rel, ok := strings.CutPrefix(fp, prefix); ok {
		return rel
	}
	return fp
}

// renderToolVerbose returns additional indented lines showing the full tool
// input and output. For the Edit tool it renders a coloured inline diff.
func (m *SessionViewModel) renderToolVerbose(p agent.Part, width int) []string {
	dim := lipgloss.NewStyle().Foreground(dimColor)
	indent := "    " // 4-space indent for verbose content

	if strings.EqualFold(p.Tool, "edit") {
		return m.renderEditDiff(p, width, indent)
	}

	// Read: summary line already has path:range — nothing more to show.
	if strings.EqualFold(p.Tool, "read") {
		return nil
	}

	// TodoWrite: render a styled checklist instead of raw JSON.
	if strings.EqualFold(p.Tool, "todowrite") {
		return renderTodoList(p, indent, dim)
	}

	var lines []string

	// Render input arguments.
	if len(p.Input) > 0 {
		lines = append(lines, dim.Render(indent+"input:"))
		lines = append(lines, renderInputMap(p.Input, width, indent+"  ", dim)...)
	}

	// Render output.
	if p.Output != "" {
		lines = append(lines, dim.Render(indent+"output:"))
		for _, ol := range strings.Split(p.Output, "\n") {
			lines = append(lines, dim.Render(indent+"  "+ol))
		}
	}

	return lines
}

// diffOp represents a single line-level diff operation.
type diffOp int

const (
	diffEqual  diffOp = iota // line is unchanged
	diffDelete               // line was removed from old
	diffInsert               // line was added in new
)

// diffLine pairs a diff operation with the line text.
type diffLine struct {
	op   diffOp
	text string
}

// diffLines computes a line-level diff between old and new using the LCS
// (longest common subsequence) algorithm. Returns a sequence of operations
// that interleaves context, deletions, and insertions — like git diff output.
func diffLines(old, new []string) []diffLine {
	n, m := len(old), len(new)

	// Build LCS table.
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if old[i-1] == new[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack to produce the diff sequence.
	var result []diffLine
	i, j := n, m
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && old[i-1] == new[j-1] {
			result = append(result, diffLine{diffEqual, old[i-1]})
			i--
			j--
		} else if j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]) {
			result = append(result, diffLine{diffInsert, new[j-1]})
			j--
		} else {
			result = append(result, diffLine{diffDelete, old[i-1]})
			i--
		}
	}

	// Reverse — backtracking produces the sequence in reverse order.
	for l, r := 0, len(result)-1; l < r; l, r = l+1, r-1 {
		result[l], result[r] = result[r], result[l]
	}
	return result
}

// highlightLinePair renders a paired delete/insert line with character-level
// highlighting. The common prefix and suffix are rendered in dim; only the
// differing middle span is colored red (old) or green (new).
func highlightLinePair(oldLine, newLine, indent string, dim, del, add lipgloss.Style) (string, string) {
	// Find common prefix length.
	pfx := 0
	for pfx < len(oldLine) && pfx < len(newLine) && oldLine[pfx] == newLine[pfx] {
		pfx++
	}
	// Find common suffix length (not overlapping prefix).
	sfx := 0
	for sfx < len(oldLine)-pfx && sfx < len(newLine)-pfx &&
		oldLine[len(oldLine)-1-sfx] == newLine[len(newLine)-1-sfx] {
		sfx++
	}

	oldMid := oldLine[pfx : len(oldLine)-sfx]
	newMid := newLine[pfx : len(newLine)-sfx]
	prefix := oldLine[:pfx]
	suffix := oldLine[len(oldLine)-sfx:]

	oldRendered := indent + "- " + dim.Render(prefix) + del.Render(oldMid) + dim.Render(suffix)
	newRendered := indent + "+ " + dim.Render(prefix) + add.Render(newMid) + dim.Render(suffix)
	return oldRendered, newRendered
}

// renderEditDiff renders an Edit tool call as a unified-style diff with
// context lines and character-level highlighting on changed lines.
func (m *SessionViewModel) renderEditDiff(p agent.Part, width int, indent string) []string {
	dim := lipgloss.NewStyle().Foreground(dimColor)
	del := lipgloss.NewStyle().Foreground(dangerColor)
	add := lipgloss.NewStyle().Foreground(successColor)

	oldStr, _ := p.Input["oldString"].(string)
	newStr, _ := p.Input["newString"].(string)

	oldLines := strings.Split(oldStr, "\n")
	newLines := strings.Split(newStr, "\n")
	ops := diffLines(oldLines, newLines)

	var lines []string
	for i := 0; i < len(ops); {
		op := ops[i]
		switch op.op {
		case diffEqual:
			lines = append(lines, dim.Render(indent+"  "+op.text))
			i++

		case diffDelete:
			// Collect consecutive delete+insert pairs for char-level highlight.
			delStart := i
			for i < len(ops) && ops[i].op == diffDelete {
				i++
			}
			insStart := i
			for i < len(ops) && ops[i].op == diffInsert {
				i++
			}
			delCount := insStart - delStart
			insCount := i - insStart

			// Pair up deletes and inserts for character-level highlighting.
			paired := delCount
			if insCount < paired {
				paired = insCount
			}
			for k := 0; k < paired; k++ {
				ol, nl := highlightLinePair(ops[delStart+k].text, ops[insStart+k].text, indent, dim, del, add)
				lines = append(lines, ol, nl)
			}
			// Remaining unpaired deletes.
			for k := paired; k < delCount; k++ {
				lines = append(lines, del.Render(indent+"- "+ops[delStart+k].text))
			}
			// Remaining unpaired inserts.
			for k := paired; k < insCount; k++ {
				lines = append(lines, add.Render(indent+"+ "+ops[insStart+k].text))
			}

		case diffInsert:
			// Pure inserts with no preceding delete.
			lines = append(lines, add.Render(indent+"+ "+op.text))
			i++
		}
	}
	return lines
}

// renderTodoList renders a TodoWrite tool call as a styled checklist.
// Each todo item shows a status icon and the content text.
func renderTodoList(p agent.Part, indent string, dim lipgloss.Style) []string {
	done := lipgloss.NewStyle().Foreground(successColor)
	prog := lipgloss.NewStyle().Foreground(warningColor)

	// Todos arrive as []interface{} from JSON-decoded Input map.
	todosRaw, _ := p.Input["todos"].([]interface{})
	if len(todosRaw) == 0 {
		return nil
	}

	var lines []string
	for _, raw := range todosRaw {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		content, _ := item["content"].(string)
		status, _ := item["status"].(string)

		var icon string
		var style lipgloss.Style
		switch status {
		case "completed":
			icon = "✓"
			style = done
		case "in_progress":
			icon = "◉"
			style = prog
		case "cancelled":
			icon = "✗"
			style = dim
		default: // pending
			icon = "○"
			style = dim
		}
		lines = append(lines, style.Render(indent+icon+" "+content))
	}
	return lines
}

// renderInputMap formats a map[string]any as indented key: value lines.
// Long string values are wrapped; non-string values are JSON-encoded.
func renderInputMap(m map[string]any, width int, indent string, style lipgloss.Style) []string {
	// Sort keys for stable output.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var lines []string
	for _, k := range keys {
		v := m[k]
		switch val := v.(type) {
		case string:
			if len(val) <= 80 && !strings.Contains(val, "\n") {
				lines = append(lines, style.Render(fmt.Sprintf("%s%s: %s", indent, k, val)))
			} else {
				lines = append(lines, style.Render(fmt.Sprintf("%s%s:", indent, k)))
				for _, sl := range strings.Split(val, "\n") {
					lines = append(lines, style.Render(indent+"  "+sl))
				}
			}
		default:
			b, err := json.Marshal(val)
			if err != nil {
				lines = append(lines, style.Render(fmt.Sprintf("%s%s: %v", indent, k, val)))
			} else {
				lines = append(lines, style.Render(fmt.Sprintf("%s%s: %s", indent, k, string(b))))
			}
		}
	}
	return lines
}

func (m *SessionViewModel) statusIcon(status agent.PartStatus) string {
	switch status {
	case agent.PartRunning:
		return m.spinner.View()
	case agent.PartCompleted:
		return lipgloss.NewStyle().Foreground(successColor).Render("✓")
	case agent.PartFailed:
		return lipgloss.NewStyle().Foreground(dangerColor).Render("✗")
	default:
		return lipgloss.NewStyle().Foreground(dimColor).Render("○")
	}
}

// isBusy returns true when the agent is actively streaming output.
func (m *SessionViewModel) isBusy() bool {
	return m.info != nil && (m.info.Status == agent.StatusBusy || m.info.Status == agent.StatusStarting)
}

func (m *SessionViewModel) sendMessage(text string) tea.Cmd {
	selectedAgent := ""
	if len(m.agents) > 0 {
		selectedAgent = m.agents[m.selectedAgent].Name
	}
	var modelOverride *agent.ModelOverride
	if m.selectedModel >= 0 && m.selectedModel < len(m.models) {
		model := m.models[m.selectedModel]
		modelOverride = &agent.ModelOverride{
			ModelID:    model.ID,
			ProviderID: model.ProviderID,
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		opts := agent.SendMessageOpts{Text: text, Agent: selectedAgent, Model: modelOverride}
		err := m.client.SendMessage(ctx, m.sessionID, opts)
		return sessionSendResultMsg{err: err}
	}
}

func (m *SessionViewModel) replyPermission(requestID string, allow bool) tea.Cmd {
	sessionID := m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := m.client.ReplyPermission(ctx, sessionID, requestID, allow)
		return sessionSendResultMsg{err: err}
	}
}

func (m *SessionViewModel) toggleFollowUp() tea.Cmd {
	client := m.client
	sessionID := m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		followUp, err := client.ToggleFollowUp(ctx, sessionID)
		return sessionFollowUpResultMsg{followUp: followUp, err: err}
	}
}

func (m *SessionViewModel) abortSession() tea.Cmd {
	client := m.client
	sessionID := m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := client.AbortSession(ctx, sessionID)
		return sessionAbortResultMsg{err: err}
	}
}

// startAbort sets up the abort state and appends a "Cancelling..." entry.
// Returns the tea.Cmd that performs the actual abort HTTP call.
func (m *SessionViewModel) startAbort() tea.Cmd {
	m.aborting = true
	m.abortEntryIdx = len(m.entries)
	m.entries = append(m.entries, displayEntry{
		kind:    entryStatus,
		content: "Cancelling...",
	})
	if m.follow {
		m.scrollToBottom()
	}
	return m.abortSession()
}

// handleCtrlCQuit implements double-tap ctrl+c to quit. On the first press
// it records the time and schedules a hint-clear; on the second press within
// 1 second it quits.
func (m *SessionViewModel) handleCtrlCQuit() (tea.Model, tea.Cmd) {
	if !m.lastCtrlC.IsZero() && time.Since(m.lastCtrlC) < time.Second {
		return m, tea.Quit
	}
	m.lastCtrlC = time.Now()
	return m, func() tea.Msg {
		time.Sleep(time.Second)
		return clearCtrlCHintMsg{}
	}
}

func (m *SessionViewModel) handleConfirmAction(action string) tea.Cmd {
	switch action {
	case "done":
		return m.setVisibility(agent.VisibilityDone)
	case "archive":
		return m.setVisibility(agent.VisibilityArchived)
	case "revert":
		return m.revertSession(m.menuMessageID)
	}
	return nil
}

func (m *SessionViewModel) handleMenuAction(action string) tea.Cmd {
	switch action {
	case "revert":
		m.showConfirm = true
		m.confirm = newConfirmDialog(
			"Revert",
			"Revert to this message?\nAll messages after it will be removed.",
			"revert",
		)
		return nil
	case "fork":
		return m.forkSession(m.menuNextMessageID)
	}
	return nil
}

// revertResultMsg carries the result of a revert operation back to the TUI.
type revertResultMsg struct {
	messageID string // the target message ID that was reverted to
	prompt    string // the user message content to prefill in the prompt
	err       error
}

// forkResultMsg carries the result of a fork operation back to the TUI.
type forkResultMsg struct {
	sessionID string // the new forked session's daemon ID
	err       error
}

// openForkedSessionMsg tells the inbox to navigate to the forked session.
type openForkedSessionMsg struct {
	sessionID string
}

func (m *SessionViewModel) revertSession(messageID string) tea.Cmd {
	client := m.client
	sessionID := m.sessionID
	prompt := m.menuMessageContent
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.RevertSession(ctx, sessionID, messageID); err != nil {
			return revertResultMsg{err: err}
		}
		return revertResultMsg{messageID: messageID, prompt: prompt}
	}
}

// forkSession creates a new session forked from the current one.
// nextMessageID is the exclusive upper bound: the fork includes all messages
// before this ID. When empty, the entire session is forked.
func (m *SessionViewModel) forkSession(nextMessageID string) tea.Cmd {
	client := m.client
	sessionID := m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		info, err := client.ForkSession(ctx, sessionID, nextMessageID)
		if err != nil {
			return forkResultMsg{err: err}
		}
		return forkResultMsg{sessionID: info.ID}
	}
}

func (m *SessionViewModel) setVisibility(visibility agent.SessionVisibility) tea.Cmd {
	client := m.client
	sessionID := m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := client.SetVisibility(ctx, sessionID, visibility)
		return sessionVisibilityResultMsg{err: err}
	}
}

// --- View ---

func (m *SessionViewModel) View() tea.View {
	// Composing mode has its own view.
	if m.composing {
		return m.viewCompose()
	}

	if m.width == 0 {
		v := newVoiceEnabledView("Loading...")
		return v
	}

	var sb strings.Builder

	// Header.
	sb.WriteString(m.renderHeader())
	sb.WriteString("\n\n")

	// Error banner.
	if m.err != nil {
		errMsg := lipgloss.NewStyle().Foreground(dangerColor).Render(fmt.Sprintf("Error: %v", m.err))
		sb.WriteString(errMsg)
		sb.WriteString("\n\n")
	}

	// Update cursor before building content so the correct entry gets the
	// selection border on this frame (avoids a one-frame layout shift).
	if m.follow {
		m.cursor = m.lastNavigableEntry()
	}

	// Content area: rendered entries.
	contentLines := m.buildContentLines()
	ch := m.contentHeight()

	// Cache for mouse selection: count how many screen rows precede the content area.
	m.cachedHeaderRows = 2 // header line + blank line
	if m.err != nil {
		m.cachedHeaderRows += 2 // error line + blank line
	}
	m.cachedContent = contentLines

	// Auto-follow: scroll to bottom.
	if m.follow {
		m.cursorMoved = false
		m.scrollOffset = len(contentLines) - ch
		if m.scrollOffset < 0 {
			m.scrollOffset = 0
		}
	} else if m.cursorMoved {
		// Keyboard navigation: position cursor entry near top of viewport.
		m.scrollToCursor()
		m.cursorMoved = false
	}
	// Otherwise (mouse scroll, no cursor change): leave scrollOffset as-is.
	m.clampScrollWithLines(contentLines)

	// Compute copy button hit region for mouse click handling.
	m.copyBtnRow = -1 // reset; set below if visible.
	if m.cursor >= 0 && m.cursor < len(m.entries) && m.entries[m.cursor].content != "" && isNavigable(m.entries[m.cursor].kind) {
		// The selected entry's rendered lines start with a blank separator,
		// so the top border line is entryStartLine[cursor] + 1.
		borderLineIdx := m.entryStartLine[m.cursor] + 1
		if borderLineIdx >= m.scrollOffset && borderLineIdx < m.scrollOffset+ch {
			screenRow := m.cachedHeaderRows + (borderLineIdx - m.scrollOffset)
			borderWidth := lipgloss.Width(contentLines[borderLineIdx])
			// Label sits at: ╭──── label ╮  →  label starts at (borderWidth - 1 - labelWidth - 1)
			// where 1 = corner char, 1 = space padding on each side.
			// "[copy]" = 6 chars, "✓ copied" = 8 chars; use the actual rendered label width.
			labelW := 6 // "[copy]"
			if !m.copiedAt.IsZero() && time.Since(m.copiedAt) < 1500*time.Millisecond && m.copiedCursor == m.cursor {
				labelW = 8 // "✓ copied"
			}
			colStart := borderWidth - 1 - 1 - labelW // skip trailing ╮ and space
			if colStart < 0 {
				colStart = 0
			}
			m.copyBtnRow = screenRow
			m.copyBtnColMin = colStart
			m.copyBtnColMax = colStart + labelW
		}
	}

	// Render visible window.
	end := m.scrollOffset + ch
	if end > len(contentLines) {
		end = len(contentLines)
	}
	for i := m.scrollOffset; i < end; i++ {
		screenRow := m.cachedHeaderRows + (i - m.scrollOffset)
		line := m.selection.highlightLine(contentLines[i], screenRow)
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	// Pad remaining lines.
	rendered := end - m.scrollOffset
	for i := rendered; i < ch; i++ {
		sb.WriteString("\n")
	}

	// Streaming spinner when busy.
	if m.info != nil && m.info.Status == agent.StatusBusy {
		sb.WriteString("  ")
		sb.WriteString(m.spinner.View())
		sb.WriteString("\n")
	}

	// Input area.
	if m.inputActive {
		sb.WriteString("\n")
		sb.WriteString(m.renderPromptBox())
		sb.WriteString("\n")
		inputHelp := "enter: send | shift+enter: newline | esc: cancel"
		if len(m.agents) > 1 {
			inputHelp = "enter: send | shift+enter: newline | tab: cycle mode | esc: cancel"
		}
		if len(m.models) > 0 {
			inputHelp += " | shift+tab: select model"
		}
		sb.WriteString(helpStyle.Render(inputHelp))
	} else {
		// Help bar.
		help := m.buildHelpText()
		sb.WriteString(helpStyle.Render(help))
	}

	output := m.overlaySessionConfirm(sb.String())
	output = m.overlaySessionMenu(output)
	output = m.overlayModelPicker(output)
	output = m.overlaySessionHelp(output)
	v := newVoiceEnabledViewWithMouse(output)
	return v
}

func (m *SessionViewModel) renderHeader() string {
	statusStr := "..."
	statusStyle := lipgloss.NewStyle().Foreground(dimColor)
	if m.info != nil {
		statusStr = string(m.info.Status)
		switch m.info.Status {
		case agent.StatusBusy:
			statusStr = m.spinner.View() + " " + statusStr
			statusStyle = lipgloss.NewStyle().Foreground(successColor).Bold(true)
		case agent.StatusIdle:
			statusStyle = lipgloss.NewStyle().Foreground(warningColor)
		case agent.StatusError, agent.StatusDead:
			statusStyle = lipgloss.NewStyle().Foreground(dangerColor)
		case agent.StatusStarting:
			statusStyle = lipgloss.NewStyle().Foreground(secondaryColor)
		}
	}

	right := statusStyle.Render("[" + statusStr + "]")

	// Show follow-up badge if flagged.
	followUpStr := ""
	if m.info != nil && m.info.FollowUp {
		followUpStr = lipgloss.NewStyle().Foreground(warningColor).Bold(true).Render("[follow-up]")
	}

	// Show agent indicator if set.
	agentStr := ""
	if len(m.agents) > 0 {
		agentName := m.agents[m.selectedAgent].Name
		agentStr = lipgloss.NewStyle().Foreground(agentColor(agentName)).Bold(true).Render(agentName)
	} else if m.info != nil && m.info.Agent != "" {
		agentStr = lipgloss.NewStyle().Foreground(agentColor(m.info.Agent)).Bold(true).Render(m.info.Agent)
	}

	// Show model indicator: user-selected override or last-used from assistant messages.
	modelStr := ""
	if m.selectedModel >= 0 && m.selectedModel < len(m.models) {
		model := m.models[m.selectedModel]
		modelStr = lipgloss.NewStyle().Foreground(secondaryColor).Render(model.ProviderID + "/" + model.ID)
	} else if m.lastProviderID != "" && m.lastModelID != "" {
		modelStr = lipgloss.NewStyle().Foreground(dimColor).Render(m.lastProviderID + "/" + m.lastModelID)
	}

	rightParts := right
	if modelStr != "" {
		rightParts = modelStr + " " + rightParts
	}
	if agentStr != "" {
		rightParts = agentStr + " " + rightParts
	}
	if followUpStr != "" {
		rightParts = followUpStr + " " + rightParts
	}
	if m.voice != nil {
		badge := voiceHeaderBadge(*m.voice)
		if badge != "" {
			rightParts = badge + " " + rightParts
		}
	}

	// Compute max title width dynamically based on available space
	// after accounting for the right-side badges and a minimum gap of 2.
	maxTitleWidth := m.width - lipgloss.Width(rightParts) - 2
	title := "Session"
	if m.info != nil {
		title = truncateStr(m.info.Prompt, maxTitleWidth)
		if m.info.Title != "" {
			title = truncateStr(m.info.Title, maxTitleWidth)
		}
	}

	left := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(title)

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(rightParts)
	if gap < 2 {
		gap = 2
	}
	return left + strings.Repeat(" ", gap) + rightParts
}

func (m *SessionViewModel) buildContentLines() []string {
	var lines []string
	m.entryStartLine = make([]int, len(m.entries))
	m.entryEndLine = make([]int, len(m.entries))
	// Track the index of the most recent navigable entry so tool entries
	// beneath it know whether they belong to the selected (cursor) entry.
	ownerIdx := -1
	for i := range m.entries {
		if isNavigable(m.entries[i].kind) {
			ownerIdx = i
		}
		// Tool entries are expanded when verbose is on and their owning
		// navigable entry is the cursor entry.
		ownerExpanded := m.verbose && ownerIdx == m.cursor
		m.entryStartLine[i] = len(lines)
		selected := i == m.cursor
		entryLines := m.renderEntry(&m.entries[i], selected, ownerExpanded)
		lines = append(lines, entryLines...)
		m.entryEndLine[i] = len(lines)
	}
	if len(lines) == 0 {
		msg := "  Waiting for agent output..."
		if !m.historyLoaded {
			msg = "  Loading conversation..."
		}
		lines = append(lines, lipgloss.NewStyle().Foreground(dimColor).Render(msg))
	}
	return lines
}

func (m *SessionViewModel) renderEntry(e *displayEntry, selected bool, ownerExpanded bool) []string {
	maxWidth := m.width - 4
	if maxWidth < 20 {
		maxWidth = 20
	}

	// borderSize accounts for the rounded border (1+1) + padding (1+1) = 4 chars.
	const borderSize = 4
	navigable := isNavigable(e.kind)

	// When selected, content is narrower to fit inside the border.
	contentWidth := maxWidth
	if selected && navigable {
		contentWidth = maxWidth - borderSize
		if contentWidth < 16 {
			contentWidth = 16
		}
	}

	var contentLines []string

	switch e.kind {
	case entryUser:
		header := lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("You:")
		if e.agent != "" {
			badge := lipgloss.NewStyle().Foreground(agentColor(e.agent)).Bold(true).Render("[" + e.agent + "]")
			header += " " + badge
		}
		wrapped := wrapText(e.content, contentWidth)
		contentLines = []string{header}
		for _, l := range strings.Split(wrapped, "\n") {
			contentLines = append(contentLines, l)
		}

	case entryText:
		header := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("Agent:")
		var rendered string
		if e.streaming {
			// While this entry is still receiving streamed deltas, use
			// plain word-wrap to avoid garbled output from incomplete
			// markdown (e.g. unclosed code fences). Previous entries
			// whose content is stable render full markdown.
			rendered = wrapText(e.content, contentWidth)
		} else {
			// Use cached glamour output when available and width hasn't changed.
			if e.renderedMD != "" && e.renderedWidth == contentWidth {
				rendered = e.renderedMD
			} else {
				rendered = renderMarkdown(e.content, contentWidth)
				e.renderedMD = rendered
				e.renderedWidth = contentWidth
			}
		}
		contentLines = []string{header}
		for _, l := range strings.Split(rendered, "\n") {
			contentLines = append(contentLines, l)
		}

	case entryTool:
		// Summary line: use cache for terminal-status tools, render live
		// for running/pending (spinner animation needs fresh renders).
		var line string
		if e.toolPart != nil {
			isTerminal := e.toolPart.Status == agent.PartCompleted || e.toolPart.Status == agent.PartFailed
			if isTerminal && e.toolLine != "" {
				line = e.toolLine
			} else {
				line = m.renderToolLine(*e.toolPart)
				if isTerminal {
					e.toolLine = line
				}
			}
		} else {
			line = e.content
		}
		styled := lipgloss.NewStyle().Foreground(dimColor).Render("  " + line)
		lines := []string{styled}

		// Show detail when owning navigable entry is expanded, or always for TodoWrite/Edit.
		// expandForceShow overrides a collapsed owner; expandForceHide overrides an expanded owner.
		// Edit diffs and TodoWrite are expanded by default; edit diffs can be collapsed via click.
		alwaysExpand := strings.EqualFold(e.toolPart.Tool, "todowrite") ||
			strings.EqualFold(e.toolPart.Tool, "edit")
		showDetail := e.toolPart != nil && ((alwaysExpand && e.expand != expandForceHide) ||
			e.expand == expandForceShow ||
			(e.expand == expandDefault && ownerExpanded))
		if showDetail {
			verboseW := maxWidth - 4
			if e.verboseLines != nil && e.verboseWidth == verboseW {
				lines = append(lines, e.verboseLines...)
			} else {
				vl := m.renderToolVerbose(*e.toolPart, verboseW)
				e.verboseLines = vl
				e.verboseWidth = verboseW
				lines = append(lines, vl...)
			}
		}
		return lines

	case entryThink:
		header := lipgloss.NewStyle().Foreground(dimColor).Italic(true).Render("(thinking)")
		wrapped := wrapText(e.content, contentWidth)
		contentLines = []string{header}
		for _, l := range strings.Split(wrapped, "\n") {
			styled := lipgloss.NewStyle().Foreground(dimColor).Italic(true).Render(l)
			contentLines = append(contentLines, styled)
		}

	case entryError:
		header := lipgloss.NewStyle().Foreground(dangerColor).Render("[error]")
		wrapped := wrapText(e.content, contentWidth)
		contentLines = []string{header}
		for _, l := range strings.Split(wrapped, "\n") {
			styled := lipgloss.NewStyle().Foreground(dangerColor).Render(l)
			contentLines = append(contentLines, styled)
		}

	case entryPerm:
		header := lipgloss.NewStyle().Foreground(warningColor).Bold(true).Render("[Permission]")
		wrapped := wrapText(e.content, contentWidth)
		contentLines = []string{header}
		for _, l := range strings.Split(wrapped, "\n") {
			styled := lipgloss.NewStyle().Foreground(warningColor).Bold(true).Render(l)
			contentLines = append(contentLines, styled)
		}

	case entryStatus:
		styled := lipgloss.NewStyle().Foreground(dimColor).Render("  --- " + e.content + " ---")
		return []string{styled}

	default:
		contentLines = []string{e.content}
	}

	if selected && navigable {
		// Wrap content in a rounded border.
		inner := strings.Join(contentLines, "\n")
		bordered := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(primaryColor).
			Padding(0, 1).
			Render(inner)
		lines := strings.Split(bordered, "\n")

		// Overlay a [copy] / ✓ copied label on the top-right of the border
		// when the entry has copyable content.
		if len(lines) > 0 && e.content != "" {
			lines[0] = m.overlayBorderLabel(lines[0])
		}

		// Prepend a blank separator line.
		return append([]string{""}, lines...)
	}

	// Unselected navigable entries: indent with 2 spaces, prepend blank separator.
	if navigable {
		lines := []string{""}
		for _, l := range contentLines {
			lines = append(lines, "  "+l)
		}
		return lines
	}

	return contentLines
}

// overlayBorderLabel splices a [copy] or ✓ copied label into the top border
// line of a selected entry, positioned at the right side just before the
// closing corner character (╮).
func (m *SessionViewModel) overlayBorderLabel(topLine string) string {
	borderStyle := lipgloss.NewStyle().Foreground(primaryColor)

	const copiedDuration = 1500 * time.Millisecond

	var label string
	if !m.copiedAt.IsZero() && time.Since(m.copiedAt) < copiedDuration && m.copiedCursor == m.cursor {
		check := lipgloss.NewStyle().Foreground(successColor).Render("✓")
		text := lipgloss.NewStyle().Foreground(successColor).Render("copied")
		label = check + " " + text
	} else {
		label = lipgloss.NewStyle().Foreground(dimColor).Render("[copy]")
	}

	// The top border line is: ╭─────...─────╮ (with ANSI color codes).
	// We measure its visual width and rebuild it with the label spliced in.
	lineWidth := lipgloss.Width(topLine)
	labelWidth := lipgloss.Width(label)

	// Need room for: ╭ + at least 1 dash + space + label + space + ╮
	minWidth := 1 + 1 + 1 + labelWidth + 1 + 1
	if lineWidth < minWidth {
		return topLine
	}

	// Number of ─ characters: total width minus corners (2) minus label minus surrounding spaces (2).
	dashCount := lineWidth - 2 - labelWidth - 2
	if dashCount < 1 {
		return topLine
	}

	corner := borderStyle.Render
	dash := borderStyle.Render("─")

	var sb strings.Builder
	sb.WriteString(corner("╭"))
	for i := 0; i < dashCount; i++ {
		sb.WriteString(dash)
	}
	sb.WriteString(corner(" "))
	sb.WriteString(label)
	sb.WriteString(corner(" ╮"))

	return sb.String()
}

func (m *SessionViewModel) buildHelpText() string {
	if !m.lastCtrlC.IsZero() && time.Since(m.lastCtrlC) < time.Second {
		return "press ctrl+c again to quit"
	}
	if len(m.pendingPerms) > 0 {
		return "y: allow | n: deny"
	}
	qLabel := "q: back"
	if m.standalone {
		qLabel = "q: quit"
	}
	parts := []string{"m: message", ":: actions", "c: copy", "?: help", qLabel}
	if m.info != nil && (m.info.Status == agent.StatusBusy || m.info.Status == agent.StatusStarting) {
		parts = append([]string{"ctrl+c: cancel"}, parts...)
	}
	if m.voice != nil {
		parts = append([]string{voiceHelpItem(*m.voice)}, parts...)
	}
	return strings.Join(parts, " | ")
}

// contentHeight returns available lines for the scrollable content area.
func (m *SessionViewModel) contentHeight() int {
	// Header (1) + blank (1) + help (2) + possible error (2) + cursor line (1) + input (5 if active)
	reserved := 5
	if m.err != nil {
		reserved += 2
	}
	if m.info != nil && m.info.Status == agent.StatusBusy {
		reserved++ // streaming cursor line
	}
	if m.inputActive {
		reserved += inputReservedLines // textarea + help
	}
	h := m.height - reserved
	if h < 3 {
		h = 3
	}
	return h
}

func (m *SessionViewModel) scrollToBottom() {
	// Will be clamped during rendering.
	m.scrollOffset = 999999
}

func (m *SessionViewModel) clampScroll() bool {
	lines := m.buildContentLines()
	return m.clampScrollWithLines(lines)
}

// clampScrollWithLines clamps scrollOffset to valid bounds and reports whether
// the viewport is now showing the very bottom of the content.
func (m *SessionViewModel) clampScrollWithLines(lines []string) bool {
	maxOffset := len(lines) - m.contentHeight()
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
	return m.scrollOffset >= maxOffset
}

// overlaySessionConfirm overlays the confirm dialog onto the base content if active.
func (m *SessionViewModel) overlaySessionConfirm(base string) string {
	if !m.showConfirm {
		return base
	}
	return overlayCenter(base, m.confirm.View(), m.width, m.height)
}

// overlaySessionMenu overlays the action menu onto the base content if active.
func (m *SessionViewModel) overlaySessionMenu(base string) string {
	if !m.showMenu {
		return base
	}
	return overlayCenter(base, m.menu.View(), m.width, m.height)
}

// overlayModelPicker overlays the model picker onto the base content if active.
func (m *SessionViewModel) overlayModelPicker(base string) string {
	if !m.showModelPicker {
		return base
	}
	return overlayCenter(base, m.modelPicker.View(), m.width, m.height)
}

// overlaySessionHelp overlays the help popup onto the base content if active.
func (m *SessionViewModel) overlaySessionHelp(base string) string {
	if !m.showHelp {
		return base
	}

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
	helpLine("shift+down/up", "jump to user messages")
	helpLine("g / G", "go to top / bottom")
	helpLine("ctrl+d / ctrl+u", "half-page down / up")
	sb.WriteString(sep + "\n")

	// Actions section.
	sb.WriteString(lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("Actions"))
	sb.WriteString("\n")
	helpLine("m", "compose message")
	helpLine(":", "actions menu")
	helpLine("v", "toggle verbose")
	helpLine("c", "copy entry")
	helpLine("o", "open in native CLI")
	sb.WriteString(sep + "\n")

	// Session management section.
	sb.WriteString(lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("Session"))
	sb.WriteString("\n")
	helpLine("f", "toggle follow-up")
	helpLine("d", "mark as done")
	helpLine("x", "archive")
	sb.WriteString(sep + "\n")

	// Voice section.
	sb.WriteString(lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("Voice"))
	sb.WriteString("\n")
	helpLine("space (hold)", "push-to-talk")
	sb.WriteString(sep + "\n")

	qLabel := "back"
	if m.standalone {
		qLabel = "quit"
	}
	helpLine("q", qLabel)

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

// --- Navigation helpers ---

// isNavigable returns true for entry kinds that the cursor can land on.
// Tool calls and status entries are skipped during navigation.
func isNavigable(k entryKind) bool {
	switch k {
	case entryUser, entryText, entryThink, entryError, entryPerm:
		return true
	default:
		return false
	}
}

// nextNavigableEntry returns the index of the next navigable entry after from,
// or -1 if there is none.
func (m *SessionViewModel) nextNavigableEntry(from int) int {
	for i := from + 1; i < len(m.entries); i++ {
		if isNavigable(m.entries[i].kind) {
			return i
		}
	}
	return -1
}

// prevNavigableEntry returns the index of the previous navigable entry before from,
// or -1 if there is none.
func (m *SessionViewModel) prevNavigableEntry(from int) int {
	for i := from - 1; i >= 0; i-- {
		if isNavigable(m.entries[i].kind) {
			return i
		}
	}
	return -1
}

// nextUserEntry returns the index of the next user message after from,
// or -1 if there is none.
func (m *SessionViewModel) nextUserEntry(from int) int {
	for i := from + 1; i < len(m.entries); i++ {
		if m.entries[i].kind == entryUser {
			return i
		}
	}
	return -1
}

// prevUserEntry returns the index of the previous user message before from,
// or -1 if there is none.
func (m *SessionViewModel) prevUserEntry(from int) int {
	for i := from - 1; i >= 0; i-- {
		if m.entries[i].kind == entryUser {
			return i
		}
	}
	return -1
}

// firstNavigableEntry returns the index of the first navigable entry, or 0.
func (m *SessionViewModel) firstNavigableEntry() int {
	for i := 0; i < len(m.entries); i++ {
		if isNavigable(m.entries[i].kind) {
			return i
		}
	}
	return 0
}

// lastNavigableEntry returns the index of the last navigable entry, or 0.
func (m *SessionViewModel) lastNavigableEntry() int {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if isNavigable(m.entries[i].kind) {
			return i
		}
	}
	return 0
}

// entryAtScreenY maps a screen row (from a mouse event) to the index of
// the entry rendered at that row. Returns -1 if no entry matches.
func (m *SessionViewModel) entryAtScreenY(screenY int) int {
	contentLine := screenY - m.cachedHeaderRows + m.scrollOffset
	if contentLine < 0 {
		return -1
	}
	for i, start := range m.entryStartLine {
		if contentLine >= start && contentLine < m.entryEndLine[i] {
			return i
		}
	}
	return -1
}

// scrollToCursor sets scrollOffset so the cursor entry's start line is near the
// top of the viewport, with a small margin. This gives a consistent reading
// position — the selected entry is always near the top with context below.
func (m *SessionViewModel) scrollToCursor() {
	if m.cursor < 0 || m.cursor >= len(m.entryStartLine) {
		return
	}
	const topMargin = 2
	m.scrollOffset = m.entryStartLine[m.cursor] - topMargin
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
	// Clamp so we don't scroll past the end of content.
	// (clampScrollWithLines will handle this in View, but be safe here.)
}

// --- Helpers ---

// wrapText wraps text at the given width, breaking on word boundaries.
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	var result strings.Builder
	for _, paragraph := range strings.Split(s, "\n") {
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			continue
		}
		lineLen := 0
		for i, w := range words {
			wLen := len(w)
			if i > 0 && lineLen+1+wLen > width {
				result.WriteString("\n")
				lineLen = 0
			} else if i > 0 {
				result.WriteString(" ")
				lineLen++
			}
			result.WriteString(w)
			lineLen += wLen
		}
	}
	return result.String()
}

func truncateStr(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// persistModelPreference saves the currently selected model to preferences.
// Safe to call from a goroutine.
func (m *SessionViewModel) persistModelPreference() {
	prefs, _ := config.LoadPreferences()
	if m.selectedModel >= 0 && m.selectedModel < len(m.models) {
		model := m.models[m.selectedModel]
		prefs.Model = &config.ModelPreference{
			ModelID:    model.ID,
			ProviderID: model.ProviderID,
		}
	} else {
		prefs.Model = nil
	}
	_ = config.SavePreferences(prefs)
}
