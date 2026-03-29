package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
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

// inboxRefreshMsg triggers a data reload from the daemon.
type inboxRefreshMsg struct{}

// inboxRow is one selectable row in the inbox.
type inboxRow struct {
	session   *agent.SessionInfo // nil for non-session rows (e.g. accordion)
	accordion string             // non-empty = archive accordion for this date group label
}

// inboxGroup is a named section of rows.
type inboxGroup struct {
	name         string
	style        lipgloss.Style
	rows         []inboxRow // active (non-archived) rows
	archivedRows []inboxRow // archived rows shown when accordion is expanded
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

	// Archive accordion state — tracks which date groups have their archive expanded.
	archiveExpanded map[string]bool // keyed by date group label

	// Confirm dialog state.
	showConfirm bool
	confirm     confirmDialogModel

	// Pre-built display data.
	displayLines []string
	rowToLine    []int

	// Session detail sub-view.
	screen       inboxScreen
	sessionView  *SessionViewModel
	activeConnID string // session ID of the detail view

	// Spinner for busy session indicators.
	spinner spinner.Model

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
	return &InboxModel{
		client:  client,
		spinner: sp,
	}
}

func (m *InboxModel) Init() tea.Cmd {
	cmds := []tea.Cmd{func() tea.Msg { return tea.RequestWindowSize }, m.discoverCmd(), m.loadDataCmd(), m.autoRefreshCmd(), m.spinner.Tick}
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

// autoRefreshCmd schedules periodic data refresh.
func (m *InboxModel) autoRefreshCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return inboxRefreshMsg{}
	})
}

func (m *InboxModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Always keep the spinner ticking, regardless of modal/screen state.
	// The spinner's tick chain is self-sustaining: each Update schedules
	// the next tick. Swallowing a single TickMsg permanently kills it.
	if tickMsg, ok := msg.(spinner.TickMsg); ok {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(tickMsg)
		return m, cmd
	}

	// If we're in session detail view (or composing), delegate.
	if m.screen == screenSession && m.sessionView != nil {
		return m.updateSessionView(msg)
	}

	// If confirm dialog is open, delegate.
	if m.showConfirm {
		return m.updateConfirm(msg)
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

	case tea.KeyPressMsg:
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
		// Refresh data and restart spinner when returning.
		return m, tea.Batch(m.loadDataCmd(), m.spinner.Tick)

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

// openComposingSession opens a composing SessionViewModel where the user
// types their first prompt. The session is created on send.
func (m *InboxModel) openComposingSession() tea.Cmd {
	m.screen = screenSession
	m.activeConnID = ""

	projectDir, _ := os.Getwd()
	m.sessionView = NewSessionViewComposing(m.client, projectDir)
	m.sessionView.width = m.width
	m.sessionView.height = m.height
	if m.width > 0 {
		m.sessionView.input.SetWidth(m.width - promptInputBorderSize)
	}
	return m.sessionView.Init()
}

func (m *InboxModel) handleInboxKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
				m.confirm = newConfirmDialog(
					"Archive Session",
					fmt.Sprintf("Archive '%s'?\nIt will be hidden from the inbox.", title),
					"archive:"+row.session.ID,
				)
			}
		}
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
// Active/attention-needing sessions float to the top; done/archived sink to the bottom.
func sessionSortPriority(s *agent.SessionInfo) int {
	switch {
	// Done/archived always sink to the bottom regardless of status.
	case s.Visibility == agent.VisibilityDone:
		return 6
	case s.Visibility == agent.VisibilityArchived:
		return 7
	case s.Status == agent.StatusBusy || s.Status == agent.StatusStarting:
		return 0
	case s.Unread():
		return 1
	case s.FollowUp:
		return 2
	case s.Status == agent.StatusError:
		return 3
	default:
		return 5 // idle, dead, etc.
	}
}

// buildGroups organises sessions into date-based groups (Today, Yesterday, …).
// Within each day, sessions are sorted by status priority then by UpdatedAt
// descending so active/attention-needing sessions appear first and
// done sessions sink to the bottom.
//
// Archived sessions are stored in each group's archivedRows and hidden
// behind a per-group accordion toggle. When a group's accordion is
// expanded (tracked by m.archiveExpanded[label]), the archived rows
// appear below the accordion in flatRows.
func (m *InboxModel) buildGroups(sessions []agent.SessionInfo) {
	now := time.Now()

	// Sort all sessions by UpdatedAt descending so day buckets are in
	// chronological order and the most recent day appears first.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	// Bucket sessions by day label, preserving insertion order.
	// Archived sessions go into a separate slice per bucket.
	type dayBucket struct {
		label    string
		rows     []inboxRow
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
		if s.Visibility == agent.VisibilityArchived {
			buckets[idx].archived = append(buckets[idx].archived, row)
		} else {
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
			archivedRows: b.archived,
		})
	}

	m.rebuildFlatRows()
}

// rebuildFlatRows reconstructs flatRows from m.groups, inserting per-group
// accordion toggles and optionally expanded archived rows.
// Called by buildGroups and when toggling an archive accordion.
func (m *InboxModel) rebuildFlatRows() {
	m.flatRows = nil
	for _, g := range m.groups {
		m.flatRows = append(m.flatRows, g.rows...)
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

func (m *InboxModel) View() tea.View {
	if m.screen == screenSession && m.sessionView != nil {
		return m.sessionView.View()
	}

	if m.width == 0 {
		v := tea.NewView("Loading...")
		v.AltScreen = true
		return v
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
			row := m.flatRows[m.cursor]
			if row.accordion != "" {
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
	help := helpStyle.Render("j/k: navigate | enter: open | n: new | f: follow-up | d: done | x: archive | r: refresh | q: quit")
	sb.WriteString(help)

	content := sb.String()

	// Overlay menu if open.
	if m.showMenu {
		content = m.overlayMenu(content)
	}

	// Overlay confirm dialog if open.
	if m.showConfirm {
		content = m.overlayConfirm(content)
	}

	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m *InboxModel) buildDisplayLines() {
	m.displayLines = nil
	m.rowToLine = make([]int, len(m.flatRows))

	flatIdx := 0
	for gi, g := range m.groups {
		// Group header.
		m.displayLines = append(m.displayLines, g.style.Render(g.name))

		// Active/done session rows.
		for ri := range g.rows {
			m.rowToLine[flatIdx] = len(m.displayLines)
			m.displayLines = append(m.displayLines, m.renderRow(g.rows[ri], false))
			flatIdx++
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
		m.displayLines = append(m.displayLines,
			lipgloss.NewStyle().Foreground(mutedColor).Render(
				"No sessions. Press 'n' to start a new session, or run 'clank code <prompt>'."))
	}
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
	maxPromptWidth := m.width - leftFixedWidth - agoWidth - draftExtra
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
	left := fmt.Sprintf("  %s %s %s %s %s",
		styledProject,
		stateIcon,
		agentBadge,
		unreadMark,
		prompt,
	)

	// Pad the gap between left content and right-aligned timestamp.
	// Use ANSI-unaware length for the visible width of left.
	leftVisible := lipgloss.Width(left)
	agoVisible := lipgloss.Width(styledAgo)
	gap := m.width - leftVisible - agoVisible
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

// groupFlatRowCount returns the number of flatRows occupied by the given group,
// including active rows, the accordion toggle (if any), and expanded archived rows.
func (m *InboxModel) groupFlatRowCount(g inboxGroup) int {
	n := len(g.rows)
	if len(g.archivedRows) > 0 {
		n++ // accordion toggle
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

// groupLastNonDoneRow returns the flatRows index of the last non-done session
// row in the given group. This is the idle/done boundary — the row right above
// the topmost done session. Returns -1 if the group has no non-done session rows.
func (m *InboxModel) groupLastNonDoneRow(groupIdx int) int {
	g := m.groups[groupIdx]
	offset := m.groupFirstRow(groupIdx)
	lastNonDone := -1
	for i, row := range g.rows {
		if row.session != nil && row.session.Visibility != agent.VisibilityDone {
			lastNonDone = offset + i
		}
	}
	return lastNonDone
}

// buildBreakpoints returns the sorted, deduplicated list of flatRow indices
// that shift+up/down should cycle through. For each date group the breakpoints
// are the first row and (if distinct) the idle/done boundary.
func (m *InboxModel) buildBreakpoints() []int {
	var bp []int
	for gi := range m.groups {
		first := m.groupFirstRow(gi)
		bp = append(bp, first)
		if boundary := m.groupLastNonDoneRow(gi); boundary >= 0 && boundary != first {
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
	return m.overlayPopup(base, m.menu.View())
}

func (m *InboxModel) overlayConfirm(base string) string {
	return m.overlayPopup(base, m.confirm.View())
}

func (m *InboxModel) overlayPopup(base string, popup string) string {
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
