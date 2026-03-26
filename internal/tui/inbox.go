package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemon"
)

// inboxScreen tracks which screen is active within the inbox app.
type inboxScreen int

const (
	screenInbox      inboxScreen = iota
	screenSession                // Viewing a specific session
	screenNewSession             // New session dialog
)

// inboxRefreshMsg triggers a data reload from the daemon.
type inboxRefreshMsg struct{}

// newSessionCreatedMsg is sent when a session has been created via the new session dialog.
type newSessionCreatedMsg struct {
	sessionID string
	events    <-chan agent.Event
	cancel    context.CancelFunc
}

// inboxRow is one selectable row in the inbox.
type inboxRow struct {
	session *agent.SessionInfo // nil if this is a placeholder or info row
}

// inboxGroup is a named section of rows.
type inboxGroup struct {
	name  string
	style lipgloss.Style
	rows  []inboxRow
}

// InboxModel is the top-level Bubble Tea model for the agent inbox.
// It lists daemon-managed sessions grouped by status and can navigate
// into a SessionViewModel for a specific session.
type InboxModel struct {
	client *daemon.Client

	// Inbox list state.
	groups       []inboxGroup
	flatRows     []inboxRow
	cursor       int
	scrollOffset int
	showMenu     bool
	menu         actionMenuModel

	// Pre-built display data.
	displayLines     []string
	rowToLine        []int
	rowToGroupHeader []int

	// Session detail sub-view.
	screen       inboxScreen
	sessionView  *SessionViewModel
	activeConnID string // session ID of the detail view

	// New session dialog.
	newSession newSessionModel

	width  int
	height int
	err    error
}

// NewInboxModel creates the inbox TUI connected to the given daemon client.
func NewInboxModel(client *daemon.Client) *InboxModel {
	return &InboxModel{
		client: client,
	}
}

// NewInboxModelWithNewSession creates the inbox TUI with the new session
// dialog already open. Used by `clank code` when no prompt is provided.
func NewInboxModelWithNewSession(client *daemon.Client) *InboxModel {
	m := NewInboxModel(client)
	m.screen = screenNewSession
	return m
}

func (m *InboxModel) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.WindowSize(), m.loadDataCmd(), m.autoRefreshCmd()}
	if m.screen == screenNewSession {
		// Initialize the new session dialog with CWD.
		projectDir, _ := os.Getwd()
		m.newSession = newNewSessionModel(projectDir)
		cmds = append(cmds, m.newSession.Init())
	}
	return tea.Batch(cmds...)
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

// autoRefreshCmd schedules periodic data refresh.
func (m *InboxModel) autoRefreshCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return inboxRefreshMsg{}
	})
}

func (m *InboxModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// If we're in session detail view, delegate.
	if m.screen == screenSession && m.sessionView != nil {
		return m.updateSessionView(msg)
	}

	// If we're in the new session dialog, delegate.
	if m.screen == screenNewSession {
		return m.updateNewSession(msg)
	}

	// If menu is open, delegate to menu.
	if m.showMenu {
		return m.updateMenu(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case inboxDataMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.err = nil
			m.buildGroups(msg.sessions)
		}
		return m, nil

	case inboxRefreshMsg:
		if m.screen == screenInbox {
			return m, tea.Batch(m.loadDataCmd(), m.autoRefreshCmd())
		}
		return m, m.autoRefreshCmd()

	case newSessionCreatedMsg:
		return m, m.openSessionWithEvents(msg.sessionID, msg.events, msg.cancel)

	case tea.KeyMsg:
		return m.handleInboxKey(msg)
	}

	return m, nil
}

func (m *InboxModel) updateSessionView(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case backToInboxMsg:
		m.screen = screenInbox
		m.sessionView = nil
		m.activeConnID = ""
		// Refresh data when returning.
		return m, m.loadDataCmd()

	case tea.WindowSizeMsg:
		// Forward to both.
		wMsg := msg.(tea.WindowSizeMsg)
		m.width = wMsg.Width
		m.height = wMsg.Height
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

func (m *InboxModel) updateNewSession(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case newSessionCancelMsg:
		m.screen = screenInbox
		return m, nil

	case newSessionLaunchMsg:
		m.screen = screenInbox
		return m, m.createAndOpenSession(msg.req)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		var cmd tea.Cmd
		m.newSession, cmd = m.newSession.Update(msg)
		return m, cmd

	default:
		var cmd tea.Cmd
		m.newSession, cmd = m.newSession.Update(msg)
		return m, cmd
	}
}

func (m *InboxModel) openNewSession() tea.Cmd {
	m.screen = screenNewSession
	// Default project dir to CWD. The user can't edit it in the dialog
	// (that's a future enhancement), but it's set correctly.
	projectDir, _ := os.Getwd()
	m.newSession = newNewSessionModel(projectDir)
	m.newSession.width = m.width
	m.newSession.height = m.height
	if m.width > 8 {
		m.newSession.prompt.SetWidth(m.width - 8)
	}
	return m.newSession.Init()
}

// createAndOpenSession creates a session via the daemon then opens the detail view.
func (m *InboxModel) createAndOpenSession(req agent.StartRequest) tea.Cmd {
	return func() tea.Msg {
		// Subscribe to SSE before creating session.
		sseCtx, sseCancel := context.WithCancel(context.Background())
		events, err := m.client.SubscribeEvents(sseCtx)
		if err != nil {
			sseCancel()
			return inboxDataMsg{err: fmt.Errorf("subscribe events: %w", err)}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		info, err := m.client.CreateSession(ctx, req)
		if err != nil {
			sseCancel()
			return inboxDataMsg{err: fmt.Errorf("create session: %w", err)}
		}

		return newSessionCreatedMsg{
			sessionID: info.ID,
			events:    events,
			cancel:    sseCancel,
		}
	}
}

func (m *InboxModel) handleInboxKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		return m, tea.Quit
	case key.Matches(msg, key.NewBinding(key.WithKeys("q"))):
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
			if row.session != nil {
				return m, m.openSession(row.session.ID)
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("n"))):
		return m, m.openNewSession()
	}
	return m, nil
}

func (m *InboxModel) openSession(sessionID string) tea.Cmd {
	m.screen = screenSession
	m.activeConnID = sessionID

	// Pre-subscribe to SSE before creating the view model to avoid missing
	// events from an already-busy session. The connect is to a local Unix
	// socket so it completes near-instantly.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	events, err := m.client.SubscribeEvents(sseCtx)

	m.sessionView = NewSessionViewModel(m.client, sessionID)
	if err == nil {
		m.sessionView.SetEventChannel(events, sseCancel)
	} else {
		// Fall back to subscribing in Init() if pre-subscribe fails.
		sseCancel()
	}

	// Forward current dimensions so the session view doesn't stay at "Loading...".
	m.sessionView.width = m.width
	m.sessionView.height = m.height
	if m.width > 4 {
		m.sessionView.input.SetWidth(m.width - 4)
	}
	return m.sessionView.Init()
}

// openSessionWithEvents opens the session detail view with a pre-connected SSE channel.
// Used after creating a session from the new session dialog — the SSE subscription
// was established before session creation to avoid missing early events.
func (m *InboxModel) openSessionWithEvents(sessionID string, events <-chan agent.Event, cancel context.CancelFunc) tea.Cmd {
	m.screen = screenSession
	m.activeConnID = sessionID

	m.sessionView = NewSessionViewModel(m.client, sessionID)
	m.sessionView.SetEventChannel(events, cancel)

	m.sessionView.width = m.width
	m.sessionView.height = m.height
	if m.width > 4 {
		m.sessionView.input.SetWidth(m.width - 4)
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

// buildGroups organizes sessions into display groups.
func (m *InboxModel) buildGroups(sessions []agent.SessionInfo) {
	var busyRows, unreadRows, idleRows, errorRows, deadRows []inboxRow

	for i := range sessions {
		s := &sessions[i]
		row := inboxRow{session: s}

		switch s.Status {
		case agent.StatusBusy, agent.StatusStarting:
			busyRows = append(busyRows, row)
		case agent.StatusIdle:
			if s.Unread() {
				unreadRows = append(unreadRows, row)
			} else {
				idleRows = append(idleRows, row)
			}
		case agent.StatusError:
			errorRows = append(errorRows, row)
		case agent.StatusDead:
			deadRows = append(deadRows, row)
		default:
			idleRows = append(idleRows, row)
		}
	}

	m.groups = nil

	if len(busyRows) > 0 {
		m.groups = append(m.groups, inboxGroup{
			name:  fmt.Sprintf("BUSY (%d)", len(busyRows)),
			style: lipgloss.NewStyle().Foreground(successColor).Bold(true),
			rows:  busyRows,
		})
	}
	if len(unreadRows) > 0 {
		m.groups = append(m.groups, inboxGroup{
			name:  fmt.Sprintf("UNREAD (%d)", len(unreadRows)),
			style: lipgloss.NewStyle().Foreground(secondaryColor).Bold(true),
			rows:  unreadRows,
		})
	}
	if len(idleRows) > 0 {
		m.groups = append(m.groups, inboxGroup{
			name:  fmt.Sprintf("IDLE (%d)", len(idleRows)),
			style: lipgloss.NewStyle().Foreground(warningColor).Bold(true),
			rows:  idleRows,
		})
	}
	if len(errorRows) > 0 {
		m.groups = append(m.groups, inboxGroup{
			name:  fmt.Sprintf("ERROR (%d)", len(errorRows)),
			style: lipgloss.NewStyle().Foreground(dangerColor).Bold(true),
			rows:  errorRows,
		})
	}
	if len(deadRows) > 0 {
		m.groups = append(m.groups, inboxGroup{
			name:  fmt.Sprintf("DEAD (%d)", len(deadRows)),
			style: lipgloss.NewStyle().Foreground(mutedColor).Bold(true),
			rows:  deadRows,
		})
	}

	// Flatten for cursor navigation.
	m.flatRows = nil
	for _, g := range m.groups {
		m.flatRows = append(m.flatRows, g.rows...)
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

func (m *InboxModel) View() string {
	if m.screen == screenSession && m.sessionView != nil {
		return m.sessionView.View()
	}

	if m.screen == screenNewSession {
		return m.newSession.View()
	}

	if m.width == 0 {
		return "Loading..."
	}

	var sb strings.Builder

	// Header.
	header := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render("CLANK  Inbox")
	sb.WriteString(header)
	sb.WriteString("\n\n")

	// Error.
	if m.err != nil {
		errMsg := lipgloss.NewStyle().Foreground(dangerColor).Render(fmt.Sprintf("Error: %v", m.err))
		sb.WriteString(errMsg)
		sb.WriteString("\n\n")
	}

	// Build display lines.
	m.buildDisplayLines()
	// Highlight selected row.
	if m.cursor >= 0 && m.cursor < len(m.flatRows) {
		lineIdx := m.rowToLine[m.cursor]
		if lineIdx < len(m.displayLines) {
			m.displayLines[lineIdx] = m.renderRow(m.flatRows[m.cursor], true)
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
	help := helpStyle.Render("j/k: navigate | enter: open | n: new | r: refresh | q: quit")
	sb.WriteString(help)

	content := sb.String()

	// Overlay menu if open.
	if m.showMenu {
		content = m.overlayMenu(content)
	}

	return content
}

func (m *InboxModel) buildDisplayLines() {
	m.displayLines = nil
	m.rowToLine = make([]int, len(m.flatRows))
	m.rowToGroupHeader = make([]int, len(m.flatRows))

	flatIdx := 0
	for gi, g := range m.groups {
		headerLine := len(m.displayLines)
		m.displayLines = append(m.displayLines, g.style.Render(g.name))

		for ri := range g.rows {
			m.rowToLine[flatIdx] = len(m.displayLines)
			m.rowToGroupHeader[flatIdx] = headerLine
			m.displayLines = append(m.displayLines, m.renderRow(g.rows[ri], false))
			flatIdx++
		}

		if gi < len(m.groups)-1 {
			m.displayLines = append(m.displayLines, "")
		}
	}

	if len(m.flatRows) == 0 {
		m.displayLines = append(m.displayLines,
			lipgloss.NewStyle().Foreground(mutedColor).Render(
				"No sessions. Press 'n' to start a new session, or run 'clank code <prompt>'."))
	}
}

func (m *InboxModel) renderRow(row inboxRow, selected bool) string {
	if row.session == nil {
		return ""
	}

	s := row.session
	ago := timeAgo(s.UpdatedAt)
	stateIcon := m.styledAgentStatus(s.Status)

	unreadMark := " "
	if s.Unread() {
		unreadMark = lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("*")
	}

	backendStr := lipgloss.NewStyle().Foreground(dimColor).Render(string(s.Backend))

	prompt := truncateStr(s.Prompt, m.width-60)
	if prompt == "" {
		prompt = "(no prompt)"
	}

	line := fmt.Sprintf("  %-12s %s %s %-8s %s  %s",
		lipgloss.NewStyle().Foreground(secondaryColor).Render(s.ProjectName),
		stateIcon,
		unreadMark,
		backendStr,
		prompt,
		lipgloss.NewStyle().Foreground(dimColor).Render(ago),
	)

	if selected {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		return prefix + line[2:]
	}
	return line
}

func (m *InboxModel) styledAgentStatus(status agent.SessionStatus) string {
	switch status {
	case agent.StatusBusy, agent.StatusStarting:
		return lipgloss.NewStyle().Foreground(successColor).Bold(true).Render("●")
	case agent.StatusIdle:
		return lipgloss.NewStyle().Foreground(warningColor).Render("○")
	case agent.StatusError:
		return lipgloss.NewStyle().Foreground(dangerColor).Render("✗")
	case agent.StatusDead:
		return lipgloss.NewStyle().Foreground(mutedColor).Render("✗")
	default:
		return lipgloss.NewStyle().Foreground(dimColor).Render("·")
	}
}

func (m *InboxModel) viewportHeight() int {
	reserved := 4
	if m.err != nil {
		reserved += 2
	}
	h := m.height - reserved
	if h < 3 {
		h = 3
	}
	return h
}

func (m *InboxModel) ensureCursorVisible() {
	if len(m.flatRows) == 0 {
		m.scrollOffset = 0
		return
	}
	vh := m.viewportHeight()
	cursorLine := m.rowToLine[m.cursor]
	groupHeader := m.rowToGroupHeader[m.cursor]

	topLine := cursorLine
	if groupHeader < topLine {
		topLine = groupHeader
	}

	if topLine < m.scrollOffset {
		m.scrollOffset = topLine
	}
	if cursorLine >= m.scrollOffset+vh {
		m.scrollOffset = cursorLine - vh + 1
	}
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
	popup := m.menu.View()
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
