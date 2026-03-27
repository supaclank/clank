package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
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

// sessionInfoMsg delivers a refreshed SessionInfo to the model.
type sessionInfoMsg struct {
	info *agent.SessionInfo
}

// agentsResultMsg carries the result of fetching available agents.
type agentsResultMsg struct {
	agents []agent.AgentInfo
}

// sessionMessagesMsg delivers the full message history to the model.
type sessionMessagesMsg struct {
	messages []agent.MessageData
}

// backToInboxMsg signals navigation back to the inbox.
type backToInboxMsg struct{}

// SessionViewModel shows a single agent session with streaming output.
// It also handles the "composing" mode where no session exists yet —
// the user types their first prompt and the session is created on send.
type SessionViewModel struct {
	client    *daemon.Client
	sessionID string
	info      *agent.SessionInfo

	// Display state.
	entries      []displayEntry // rendered message/tool entries
	scrollOffset int            // first visible display line
	follow       bool           // auto-follow tail when true (default when busy)

	// Input state.
	inputActive bool
	input       textarea.Model

	// Permission state.
	pendingPerm *agent.PermissionData

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

	// standalone is true when this model is run directly (not inside InboxModel).
	// When true, 'q' quits the program instead of emitting backToInboxMsg.
	standalone bool

	cancelEvents context.CancelFunc

	// Composing mode — no daemon session yet. The user is writing their
	// first prompt. After sending, this transitions to the normal session view.
	composing  bool
	backend    agent.BackendType
	projectDir string

	// Agent selection — populated eagerly when compose view loads.
	// For existing sessions opened from inbox, agents are fetched on Init.
	agents        []agent.AgentInfo
	selectedAgent int // index into agents slice
}

// displayEntry is a rendered item in the session transcript.
type displayEntry struct {
	kind    entryKind
	partID  string // Part ID for tracking updates (text deltas, tool status)
	content string // pre-rendered line(s), may contain ANSI
}

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

// DraftText returns the current unsent text in the input textarea.
func (m *SessionViewModel) DraftText() string {
	return m.input.Value()
}

// RestoreDraft sets the textarea content to the given draft text and
// activates the input so the user can continue typing immediately.
func (m *SessionViewModel) RestoreDraft(text string) {
	m.input.SetValue(text)
	m.inputActive = true
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
		return tea.Batch(m.input.Focus(), m.fetchAgents())
	}
	cmds := []tea.Cmd{m.fetchSessionInfo(), m.fetchSessionMessages(), m.spinner.Tick}
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

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// SetWidth takes the textarea's own outer width. Since we render the
		// border externally, subtract the external frame.
		m.input.SetWidth(m.width - promptInputBorderSize)
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case sessionInfoMsg:
		m.info = msg.info
		// Add the initial prompt as the first user message if entries are empty
		// and history hasn't been loaded yet (history will provide the full picture).
		if len(m.entries) == 0 && !m.historyLoaded && m.info.Prompt != "" {
			m.entries = append(m.entries, displayEntry{
				kind:    entryUser,
				content: m.info.Prompt,
			})
		}
		// Fetch agents if we don't have them yet (existing sessions opened from inbox).
		if len(m.agents) == 0 && m.info.Backend == agent.BackendOpenCode && m.info.ProjectDir != "" {
			m.backend = m.info.Backend
			m.projectDir = m.info.ProjectDir
			// Restore the selected agent from session info.
			if m.info.Agent != "" {
				// Will be matched against the fetched list in agentsResultMsg.
			}
			return m, m.fetchAgents()
		}
		return m, nil

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

	case sessionMessagesMsg:
		m.handleSessionMessages(msg.messages)
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

	case sessionEventsErrMsg:
		m.err = msg.err
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
	// Permission prompt takes priority.
	if m.pendingPerm != nil {
		switch msg.String() {
		case "y":
			perm := m.pendingPerm
			m.pendingPerm = nil
			m.entries = append(m.entries, displayEntry{
				kind:    entryStatus,
				content: "Permission granted: " + perm.Tool,
			})
			return m, m.replyPermission(perm.RequestID, "allow")
		case "n":
			perm := m.pendingPerm
			m.pendingPerm = nil
			m.entries = append(m.entries, displayEntry{
				kind:    entryStatus,
				content: "Permission denied: " + perm.Tool,
			})
			return m, m.replyPermission(perm.RequestID, "deny")
		}
		return m, nil
	}

	// Input mode.
	if m.inputActive {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.inputActive = false
			m.input.Blur()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
			// Cycle through agents.
			if len(m.agents) > 1 {
				m.selectedAgent = (m.selectedAgent + 1) % len(m.agents)
			}
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			// Send the message. Shift+enter inserts newline (handled by textarea).
			text := strings.TrimSpace(m.input.Value())
			if text != "" {
				m.entries = append(m.entries, displayEntry{
					kind:    entryUser,
					content: text,
				})
				m.input.Reset()
				m.inputActive = false
				m.input.Blur()
				m.follow = true
				return m, m.sendMessage(text)
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// Normal mode.
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		return m, tea.Quit
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
		m.input.Focus()
		return m, m.input.Focus()
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
		m.follow = false
		if m.scrollOffset > 0 {
			m.scrollOffset--
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
		m.scrollOffset++
		m.clampScroll()
	case key.Matches(msg, key.NewBinding(key.WithKeys("pgup", "ctrl+u"))):
		m.follow = false
		m.scrollOffset -= m.contentHeight() / 2
		if m.scrollOffset < 0 {
			m.scrollOffset = 0
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown", "ctrl+d"))):
		m.scrollOffset += m.contentHeight() / 2
		m.clampScroll()
	case key.Matches(msg, key.NewBinding(key.WithKeys("G", "end"))):
		m.follow = true
		m.scrollToBottom()
	case key.Matches(msg, key.NewBinding(key.WithKeys("g", "home"))):
		m.follow = false
		m.scrollOffset = 0
	case key.Matches(msg, key.NewBinding(key.WithKeys("a"))):
		// TODO: approve session
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
			m.pendingPerm = &data
			m.entries = append(m.entries, displayEntry{
				kind:    entryPerm,
				content: fmt.Sprintf("Allow %s: %s? [y/n]", data.Tool, data.Description),
			})
			if m.follow {
				m.scrollToBottom()
			}
		}

	case agent.EventError:
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
	}
}

func (m *SessionViewModel) handleStatusChange(data agent.StatusChangeData) {
	if m.info != nil {
		m.info.Status = data.NewStatus
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
		// Skip user messages from SSE — the initial prompt is already
		// added from sessionInfoMsg. Follow-up messages are added inline
		// when the user sends them via the input box.
		return
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
func (m *SessionViewModel) handleSessionMessages(messages []agent.MessageData) {
	m.seenParts = make(map[string]bool)
	m.entries = nil

	for _, msg := range messages {
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
					kind:    entryUser,
					content: text,
				})
			}
			continue
		}

		// Assistant message: render parts.
		if msg.Content != "" && len(msg.Parts) == 0 {
			m.entries = append(m.entries, displayEntry{
				kind:    entryText,
				content: msg.Content,
			})
		}
		for _, p := range msg.Parts {
			if p.ID != "" {
				m.seenParts[p.ID] = true
			}
			m.addPartEntry(p)
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
	m.upsertPartEntry(data.Part)
	if m.follow {
		m.scrollToBottom()
	}
}

// upsertPartEntry updates an existing entry with the same Part ID, or appends a new one.
// For text parts, deltas are accumulated (appended) to the existing entry's content.
// For tool parts, the entry is replaced with the new status/text.
func (m *SessionViewModel) upsertPartEntry(p agent.Part) {
	if p.ID != "" {
		for i := len(m.entries) - 1; i >= 0; i-- {
			e := &m.entries[i]
			if e.partID == p.ID {
				switch p.Type {
				case agent.PartToolCall, agent.PartToolResult:
					e.content = m.renderToolLine(p)
				case agent.PartText:
					// Accumulate text deltas into the same entry.
					e.content += p.Text
				case agent.PartThinking:
					e.content += p.Text
				}
				return
			}
		}
	}
	m.addPartEntry(p)
}

func (m *SessionViewModel) addPartEntry(p agent.Part) {
	switch p.Type {
	case agent.PartToolCall, agent.PartToolResult:
		m.entries = append(m.entries, displayEntry{
			kind:    entryTool,
			partID:  p.ID,
			content: m.renderToolLine(p),
		})
	case agent.PartThinking:
		if p.Text != "" {
			m.entries = append(m.entries, displayEntry{
				kind:    entryThink,
				partID:  p.ID,
				content: p.Text,
			})
		}
	case agent.PartText:
		m.entries = append(m.entries, displayEntry{
			kind:    entryText,
			partID:  p.ID,
			content: p.Text,
		})
	}
}

func (m *SessionViewModel) renderToolLine(p agent.Part) string {
	icon := m.statusIcon(p.Status)
	label := p.Tool
	if label == "" {
		label = string(p.Type)
	}
	desc := p.Text
	if len(desc) > 60 {
		desc = desc[:57] + "..."
	}
	statusStr := string(p.Status)
	if statusStr == "" {
		statusStr = "pending"
	}
	if desc != "" {
		return fmt.Sprintf("[%s] %s %s %s", label, desc, icon, statusStr)
	}
	return fmt.Sprintf("[%s] %s %s", label, icon, statusStr)
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

func (m *SessionViewModel) sendMessage(text string) tea.Cmd {
	selectedAgent := ""
	if len(m.agents) > 0 {
		selectedAgent = m.agents[m.selectedAgent].Name
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		opts := agent.SendMessageOpts{Text: text, Agent: selectedAgent}
		err := m.client.SendMessage(ctx, m.sessionID, opts)
		return sessionSendResultMsg{err: err}
	}
}

func (m *SessionViewModel) replyPermission(requestID, reply string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := m.client.ReplyPermission(ctx, requestID, reply)
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

func (m *SessionViewModel) handleConfirmAction(action string) tea.Cmd {
	switch action {
	case "done":
		return m.setVisibility(agent.VisibilityDone)
	case "archive":
		return m.setVisibility(agent.VisibilityArchived)
	}
	return nil
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
		v := tea.NewView("Loading...")
		v.AltScreen = true
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

	// Content area: rendered entries.
	contentLines := m.buildContentLines()
	ch := m.contentHeight()

	// Auto-follow to bottom.
	if m.follow {
		m.scrollOffset = len(contentLines) - ch
		if m.scrollOffset < 0 {
			m.scrollOffset = 0
		}
	}
	m.clampScrollWithLines(contentLines)

	// Render visible window.
	end := m.scrollOffset + ch
	if end > len(contentLines) {
		end = len(contentLines)
	}
	for i := m.scrollOffset; i < end; i++ {
		sb.WriteString(contentLines[i])
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
		sb.WriteString(helpStyle.Render(inputHelp))
	} else {
		// Help bar.
		help := m.buildHelpText()
		sb.WriteString(helpStyle.Render(help))
	}

	v := tea.NewView(m.overlaySessionConfirm(sb.String()))
	v.AltScreen = true
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

	rightParts := right
	if agentStr != "" {
		rightParts = agentStr + " " + right
	}
	if followUpStr != "" {
		rightParts = followUpStr + " " + rightParts
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
	for _, e := range m.entries {
		entryLines := m.renderEntry(e)
		lines = append(lines, entryLines...)
	}
	if len(lines) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(dimColor).Render("  Waiting for agent output..."))
	}
	return lines
}

func (m *SessionViewModel) renderEntry(e displayEntry) []string {
	maxWidth := m.width - 4
	if maxWidth < 20 {
		maxWidth = 20
	}

	switch e.kind {
	case entryUser:
		header := lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("You:")
		wrapped := wrapText(e.content, maxWidth)
		lines := []string{"", "  " + header}
		for _, l := range strings.Split(wrapped, "\n") {
			lines = append(lines, "  "+l)
		}
		return lines

	case entryText:
		header := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("Agent:")
		wrapped := wrapText(e.content, maxWidth)
		lines := []string{"", "  " + header}
		for _, l := range strings.Split(wrapped, "\n") {
			lines = append(lines, "  "+l)
		}
		return lines

	case entryTool:
		styled := lipgloss.NewStyle().Foreground(dimColor).Render("  " + e.content)
		return []string{styled}

	case entryThink:
		header := lipgloss.NewStyle().Foreground(dimColor).Italic(true).Render("  (thinking)")
		wrapped := wrapText(e.content, maxWidth)
		lines := []string{header}
		for _, l := range strings.Split(wrapped, "\n") {
			styled := lipgloss.NewStyle().Foreground(dimColor).Italic(true).Render("  " + l)
			lines = append(lines, styled)
		}
		return lines

	case entryError:
		styled := lipgloss.NewStyle().Foreground(dangerColor).Render("  [error] " + e.content)
		return []string{styled}

	case entryPerm:
		styled := lipgloss.NewStyle().Foreground(warningColor).Bold(true).Render("  [Permission] " + e.content)
		return []string{"", styled}

	case entryStatus:
		styled := lipgloss.NewStyle().Foreground(dimColor).Render("  --- " + e.content + " ---")
		return []string{styled}

	default:
		return []string{"  " + e.content}
	}
}

func (m *SessionViewModel) buildHelpText() string {
	if m.pendingPerm != nil {
		return "y: allow | n: deny"
	}
	qLabel := "q: back"
	if m.standalone {
		qLabel = "q: quit"
	}
	parts := []string{"m: message", "f: follow-up", "d: done", "x: archive", qLabel}
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
		reserved += 6 // textarea + help
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

func (m *SessionViewModel) clampScroll() {
	lines := m.buildContentLines()
	m.clampScrollWithLines(lines)
}

func (m *SessionViewModel) clampScrollWithLines(lines []string) {
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
}

// overlaySessionConfirm overlays the confirm dialog onto the base content if active.
func (m *SessionViewModel) overlaySessionConfirm(base string) string {
	if !m.showConfirm {
		return base
	}

	popup := m.confirm.View()
	popupLines := strings.Split(popup, "\n")
	baseLines := strings.Split(base, "\n")

	popupH := len(popupLines)
	popupW := 0
	for _, l := range popupLines {
		if w := lipgloss.Width(l); w > popupW {
			popupW = w
		}
	}

	startRow := (m.height - popupH) / 2
	if startRow < 0 {
		startRow = 0
	}
	startCol := (m.width - popupW) / 2
	if startCol < 0 {
		startCol = 0
	}

	for len(baseLines) < startRow+popupH {
		baseLines = append(baseLines, "")
	}

	for i, popLine := range popupLines {
		row := startRow + i
		if row >= len(baseLines) {
			break
		}
		baseLine := baseLines[row]
		for lipgloss.Width(baseLine) < startCol {
			baseLine += " "
		}
		baseLines[row] = baseLine[:startCol] + popLine
	}

	return strings.Join(baseLines, "\n")
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
