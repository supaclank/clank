package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
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

// sessionInfoMsg delivers a refreshed SessionInfo to the model.
type sessionInfoMsg struct {
	info *agent.SessionInfo
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

	// Layout.
	width  int
	height int
	err    error

	// standalone is true when this model is run directly (not inside InboxModel).
	// When true, 'q' quits the program instead of emitting backToInboxMsg.
	standalone bool

	cancelEvents context.CancelFunc

	// Composing mode — no daemon session yet. The user is writing their
	// first prompt. After sending, this transitions to the normal session view.
	composing  bool
	backend    agent.BackendType
	projectDir string
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
	return &SessionViewModel{
		client:    client,
		sessionID: sessionID,
		follow:    true,
		input:     ta,
	}
}

// SetStandalone marks this view as running standalone (not inside InboxModel).
// When standalone, 'q' quits the program instead of navigating back to inbox.
func (m *SessionViewModel) SetStandalone(v bool) {
	m.standalone = v
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
		return m.input.Focus()
	}
	cmds := []tea.Cmd{m.fetchSessionInfo()}
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

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// SetWidth takes total outer width (including border/padding),
		// so pass the full width — the textarea handles frame internally.
		m.input.SetWidth(m.width)
		return m, nil

	case sessionInfoMsg:
		m.info = msg.info
		// Add the initial prompt as the first user message if entries are empty.
		if len(m.entries) == 0 && m.info.Prompt != "" {
			m.entries = append(m.entries, displayEntry{
				kind:    entryUser,
				content: m.info.Prompt,
			})
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
		if msg.err != nil {
			m.err = msg.err
		}
		return m, nil

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
	case key.Matches(msg, key.NewBinding(key.WithKeys("q"))):
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
		// TODO: mark follow-up
	case key.Matches(msg, key.NewBinding(key.WithKeys("x"))):
		// TODO: archive session
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

func (m *SessionViewModel) handlePartUpdate(data agent.PartUpdateData) {
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
		return lipgloss.NewStyle().Foreground(successColor).Render("●")
	case agent.PartCompleted:
		return lipgloss.NewStyle().Foreground(successColor).Render("✓")
	case agent.PartFailed:
		return lipgloss.NewStyle().Foreground(dangerColor).Render("✗")
	default:
		return lipgloss.NewStyle().Foreground(dimColor).Render("○")
	}
}

func (m *SessionViewModel) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := m.client.SendMessage(ctx, m.sessionID, text)
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

	// Streaming cursor when busy.
	if m.info != nil && m.info.Status == agent.StatusBusy {
		sb.WriteString(lipgloss.NewStyle().Foreground(successColor).Bold(true).Render("  ▊"))
		sb.WriteString("\n")
	}

	// Input area.
	if m.inputActive {
		sb.WriteString("\n")
		sb.WriteString(m.input.View())
		sb.WriteString("\n")
		sb.WriteString(helpStyle.Render("enter: send | shift+enter: newline | esc: cancel"))
	} else {
		// Help bar.
		help := m.buildHelpText()
		sb.WriteString(helpStyle.Render(help))
	}

	v := tea.NewView(sb.String())
	v.AltScreen = true
	return v
}

func (m *SessionViewModel) renderHeader() string {
	title := "Session"
	if m.info != nil {
		title = truncateStr(m.info.Prompt, 50)
	}

	statusStr := "..."
	statusStyle := lipgloss.NewStyle().Foreground(dimColor)
	if m.info != nil {
		statusStr = string(m.info.Status)
		switch m.info.Status {
		case agent.StatusBusy:
			statusStyle = lipgloss.NewStyle().Foreground(successColor).Bold(true)
		case agent.StatusIdle:
			statusStyle = lipgloss.NewStyle().Foreground(warningColor)
		case agent.StatusError, agent.StatusDead:
			statusStyle = lipgloss.NewStyle().Foreground(dangerColor)
		case agent.StatusStarting:
			statusStyle = lipgloss.NewStyle().Foreground(secondaryColor)
		}
	}

	left := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render(title)
	right := statusStyle.Render("[" + statusStr + "]")
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		gap = 2
	}
	return left + strings.Repeat(" ", gap) + right
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
	parts := []string{"m: message", qLabel}
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
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
