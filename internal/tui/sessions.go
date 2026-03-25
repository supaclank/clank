package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/acksell/clank/internal/scanner/opencode"
	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/internal/terminal"
)

// sessionsRow is a unified row in the sessions kanban view.
// It can represent either a session or a backlog ticket.
type sessionsRow struct {
	// Session fields (set when isTicket == false)
	sessionID string
	title     string
	repoName  string
	updatedAt time.Time
	state     store.SessionState // effective state (from DB or default)
	unread    bool               // true if agent responded and user hasn't opened it

	// Ticket fields (set when isTicket == true)
	isTicket bool
	ticket   *store.Ticket
}

// sessionsGroup is a named group of rows (one kanban column).
type sessionsGroup struct {
	name  string
	style lipgloss.Style
	rows  []sessionsRow
}

// SessionsModel is the Bubble Tea model for the sessions kanban view.
type SessionsModel struct {
	store   *store.Store
	scanner *opencode.Scanner

	groups       []sessionsGroup
	flatRows     []sessionsRow // flattened for cursor navigation
	cursor       int
	scrollOffset int // first visible display line index
	showMenu     bool
	menu         actionMenuModel
	sessionLimit int

	// Pre-built display lines and mapping from flatRow index → displayLine index.
	displayLines     []string
	rowToLine        []int // rowToLine[flatRowIdx] = displayLineIdx
	rowToGroupHeader []int // rowToGroupHeader[flatRowIdx] = display line of that row's group header

	width  int
	height int
	err    error
}

// tickMsg triggers a data refresh.
type tickMsg struct{}

// openSessionMsg triggers opening a session in a new terminal.
type openSessionMsg struct {
	sessionID string
}

// NewSessionsModel creates the sessions kanban TUI.
func NewSessionsModel(s *store.Store, sc *opencode.Scanner, sessionLimit int) *SessionsModel {
	return &SessionsModel{
		store:        s,
		scanner:      sc,
		sessionLimit: sessionLimit,
	}
}

func (m *SessionsModel) Init() tea.Cmd {
	return tea.Batch(tea.WindowSize(), m.refreshCmd())
}

func (m *SessionsModel) refreshCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

func (m *SessionsModel) loadData() {
	// 1. Load recent sessions from OpenCode DB.
	sessions, err := m.scanner.ListRecentSessions(m.sessionLimit)
	if err != nil {
		m.err = err
		return
	}

	// 2. Load all session statuses from Clank DB.
	statuses, err := m.store.ListSessionStatuses()
	if err != nil {
		m.err = err
		return
	}

	// 3. Build session rows with effective state.
	var idleRows, busyRows, errorRows, followupRows []sessionsRow
	for _, sess := range sessions {
		state := store.SessionIdle // default: sessions without plugin tracking are "idle"
		unread := false
		if st, ok := statuses[sess.ID]; ok {
			state = st.Status
			unread = st.Unread
		}

		// Skip approved/archived sessions.
		if state == store.SessionApproved || state == store.SessionArchived {
			continue
		}

		row := sessionsRow{
			sessionID: sess.ID,
			title:     sess.Title,
			repoName:  sess.RepoName,
			updatedAt: sess.UpdatedAt,
			state:     state,
			unread:    unread,
		}

		switch state {
		case store.SessionBusy:
			busyRows = append(busyRows, row)
		case store.SessionError:
			errorRows = append(errorRows, row)
		case store.SessionFollowup:
			followupRows = append(followupRows, row)
		default: // idle
			idleRows = append(idleRows, row)
		}
	}

	// 4. Load top 10 backlog tickets.
	tickets, _ := m.store.TopTicketsByImpact(10)
	var ticketRows []sessionsRow
	for i := range tickets {
		ticketRows = append(ticketRows, sessionsRow{
			isTicket: true,
			ticket:   &tickets[i],
			repoName: filepath.Base(tickets[i].RepoPath),
			title:    tickets[i].Title,
		})
	}

	// 5. Build groups.
	m.groups = nil
	if len(idleRows) > 0 {
		m.groups = append(m.groups, sessionsGroup{
			name:  fmt.Sprintf("IDLE (%d%s) — agent finished, needs review", len(idleRows), unreadSuffix(idleRows)),
			style: lipgloss.NewStyle().Foreground(warningColor).Bold(true),
			rows:  idleRows,
		})
	}
	if len(busyRows) > 0 {
		m.groups = append(m.groups, sessionsGroup{
			name:  fmt.Sprintf("BUSY (%d)", len(busyRows)),
			style: lipgloss.NewStyle().Foreground(successColor).Bold(true),
			rows:  busyRows,
		})
	}
	if len(errorRows) > 0 {
		m.groups = append(m.groups, sessionsGroup{
			name:  fmt.Sprintf("ERROR (%d%s)", len(errorRows), unreadSuffix(errorRows)),
			style: lipgloss.NewStyle().Foreground(dangerColor).Bold(true),
			rows:  errorRows,
		})
	}
	if len(followupRows) > 0 {
		m.groups = append(m.groups, sessionsGroup{
			name:  fmt.Sprintf("FOLLOW-UP (%d%s)", len(followupRows), unreadSuffix(followupRows)),
			style: lipgloss.NewStyle().Foreground(secondaryColor).Bold(true),
			rows:  followupRows,
		})
	}
	if len(ticketRows) > 0 {
		m.groups = append(m.groups, sessionsGroup{
			name:  "BACKLOG (top 10 by impact)",
			style: lipgloss.NewStyle().Foreground(mutedColor).Bold(true),
			rows:  ticketRows,
		})
	}

	// 6. Flatten rows for cursor navigation.
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

func (m *SessionsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// If menu is open, delegate to menu first.
	if m.showMenu {
		return m.updateMenu(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.loadData()

	case tickMsg:
		m.loadData()
		return m, m.refreshCmd()

	case tea.KeyMsg:
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
			// Manual refresh.
			m.loadData()
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			if m.cursor >= 0 && m.cursor < len(m.flatRows) {
				m.openMenuForRow(m.flatRows[m.cursor])
			}
		}

	case openSessionMsg:
		if err := terminal.OpenSession(msg.sessionID); err != nil {
			m.err = err
		}
	}

	return m, nil
}

func (m *SessionsModel) updateMenu(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (m *SessionsModel) openMenuForRow(row sessionsRow) {
	if row.isTicket {
		// Ticket actions -- open the related session if one exists.
		items := []actionMenuItem{}
		if row.ticket.SessionID != "" {
			items = append(items, actionMenuItem{
				label:  "Open session in Ghostty",
				key:    "o",
				action: "open:" + row.ticket.SessionID,
			})
			items = append(items, actionMenuItem{separator: true})
		}
		items = append(items,
			actionMenuItem{label: "Move to doing", key: "d", action: "ticket-doing:" + row.ticket.ID},
			actionMenuItem{label: "Move to done", key: "x", action: "ticket-done:" + row.ticket.ID},
		)
		m.menu = newActionMenu(truncate(row.title, 32), items)
		m.showMenu = true
		return
	}

	// Session actions.
	items := []actionMenuItem{
		{label: "Open in Ghostty", key: "o", action: "open:" + row.sessionID},
	}
	items = append(items, actionMenuItem{separator: true})

	switch row.state {
	case store.SessionBusy:
		// Can't approve/archive a busy session; only open.
	case store.SessionError:
		items = append(items,
			actionMenuItem{label: "Follow-up", key: "f", action: "followup:" + row.sessionID},
			actionMenuItem{label: "Archive", key: "a", action: "archive:" + row.sessionID},
		)
	default: // idle, followup
		items = append(items,
			actionMenuItem{label: "Approve (tested/QA'd)", key: "v", action: "approve:" + row.sessionID},
			actionMenuItem{label: "Follow-up", key: "f", action: "followup:" + row.sessionID},
			actionMenuItem{label: "Archive", key: "a", action: "archive:" + row.sessionID},
		)
	}

	m.menu = newActionMenu(truncate(row.title, 32), items)
	m.showMenu = true
}

func (m *SessionsModel) handleMenuAction(action string) tea.Cmd {
	parts := strings.SplitN(action, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	verb, id := parts[0], parts[1]

	switch verb {
	case "open":
		m.store.MarkSessionRead(id)
		return func() tea.Msg { return openSessionMsg{sessionID: id} }
	case "approve":
		m.store.SetSessionStatus(id, store.SessionApproved, "opencode")
		m.loadData()
	case "followup":
		m.store.SetSessionStatus(id, store.SessionFollowup, "opencode")
		m.loadData()
	case "archive":
		m.store.SetSessionStatus(id, store.SessionArchived, "opencode")
		m.loadData()
	case "ticket-doing":
		m.setTicketStatus(id, store.StatusDoing)
	case "ticket-done":
		m.setTicketStatus(id, store.StatusDone)
	}
	return nil
}

func (m *SessionsModel) setTicketStatus(ticketID string, status store.TicketStatus) {
	t, err := m.store.GetTicket(ticketID)
	if err != nil {
		return
	}
	t.Status = status
	m.store.SaveTicket(t)
	m.loadData()
}

// viewportHeight returns the number of lines available for the scrollable content area.
// Reserves lines for the header (2), help bar (2), and optional error (2).
func (m *SessionsModel) viewportHeight() int {
	reserved := 4 // header line + blank + help line + margin
	if m.err != nil {
		reserved += 2
	}
	h := m.height - reserved
	if h < 3 {
		h = 3
	}
	return h
}

// buildDisplayLines pre-renders all rows into display lines and records
// the mapping from flat row index to display line index.
func (m *SessionsModel) buildDisplayLines() {
	m.displayLines = nil
	m.rowToLine = make([]int, len(m.flatRows))
	m.rowToGroupHeader = make([]int, len(m.flatRows))

	flatIdx := 0
	for gi, g := range m.groups {
		// Group header.
		headerLine := len(m.displayLines)
		m.displayLines = append(m.displayLines, g.style.Render(g.name))

		for ri := range g.rows {
			m.rowToLine[flatIdx] = len(m.displayLines)
			m.rowToGroupHeader[flatIdx] = headerLine
			m.displayLines = append(m.displayLines, m.renderRow(g.rows[ri], false))
			flatIdx++
		}

		// Blank line between groups (but not after the last group).
		if gi < len(m.groups)-1 {
			m.displayLines = append(m.displayLines, "")
		}
	}

	if len(m.flatRows) == 0 {
		m.displayLines = append(m.displayLines,
			lipgloss.NewStyle().Foreground(mutedColor).Render(
				"No sessions found. Start an opencode session, or run 'clank scan' to populate the backlog."))
	}
}

// ensureCursorVisible adjusts scrollOffset so the cursor row is on-screen.
// When scrolling up, it also reveals the group header above the cursor.
func (m *SessionsModel) ensureCursorVisible() {
	if len(m.flatRows) == 0 {
		m.scrollOffset = 0
		return
	}
	vh := m.viewportHeight()
	cursorLine := m.rowToLine[m.cursor]
	groupHeader := m.rowToGroupHeader[m.cursor]

	// When scrolling up, show the group header if it's just above the viewport.
	topLine := cursorLine
	if groupHeader < topLine {
		topLine = groupHeader
	}

	// Scroll up if cursor (or its group header) is above the viewport.
	if topLine < m.scrollOffset {
		m.scrollOffset = topLine
	}
	// Scroll down if cursor is below the viewport.
	if cursorLine >= m.scrollOffset+vh {
		m.scrollOffset = cursorLine - vh + 1
	}
	// Clamp.
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

func (m *SessionsModel) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	var sb strings.Builder

	header := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render("CLANK  Sessions")
	sb.WriteString(header)
	sb.WriteString("\n\n")

	if m.err != nil {
		errMsg := lipgloss.NewStyle().Foreground(dangerColor).Render(fmt.Sprintf("Error: %v", m.err))
		sb.WriteString(errMsg)
		sb.WriteString("\n\n")
	}

	// Rebuild display lines with current cursor highlighting.
	m.buildDisplayLines()
	// Mark the selected row.
	if m.cursor >= 0 && m.cursor < len(m.flatRows) {
		lineIdx := m.rowToLine[m.cursor]
		if lineIdx < len(m.displayLines) {
			m.displayLines[lineIdx] = m.renderRow(m.flatRows[m.cursor], true)
		}
	}
	m.ensureCursorVisible()

	// Render the visible window of display lines.
	vh := m.viewportHeight()
	end := m.scrollOffset + vh
	if end > len(m.displayLines) {
		end = len(m.displayLines)
	}
	for i := m.scrollOffset; i < end; i++ {
		sb.WriteString(m.displayLines[i])
		sb.WriteString("\n")
	}
	// Pad remaining viewport lines so the help bar stays at the bottom.
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

	// Help.
	help := helpStyle.Render("j/k: navigate | enter: actions | r: refresh | q: quit")
	sb.WriteString(help)

	content := sb.String()

	// Overlay menu if open.
	if m.showMenu {
		content = m.overlayMenu(content)
	}

	return content
}

func (m *SessionsModel) renderRow(row sessionsRow, selected bool) string {
	var line string

	if row.isTicket {
		t := row.ticket
		q := string(t.Quadrant())
		if q == "" {
			q = "—"
		}
		impact := fmt.Sprintf("I:%d", t.Impact)
		line = fmt.Sprintf("  %-12s %-11s %-6s %s",
			lipgloss.NewStyle().Foreground(secondaryColor).Render(row.repoName),
			lipgloss.NewStyle().Foreground(mutedColor).Render(q),
			lipgloss.NewStyle().Foreground(dimColor).Render(impact),
			truncate(row.title, m.width-44),
		)
	} else {
		ago := timeAgo(row.updatedAt)
		stateStr := m.styledState(row.state)
		unreadMark := " "
		if row.unread {
			unreadMark = lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("*")
		}
		line = fmt.Sprintf("  %-12s %s %s %s  %s",
			lipgloss.NewStyle().Foreground(secondaryColor).Render(row.repoName),
			stateStr,
			unreadMark,
			truncate(row.title, m.width-52),
			lipgloss.NewStyle().Foreground(dimColor).Render(ago),
		)
	}

	if selected {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		return prefix + line[2:] // replace the leading spaces with cursor
	}
	return line
}

func (m *SessionsModel) styledState(state store.SessionState) string {
	switch state {
	case store.SessionBusy:
		return lipgloss.NewStyle().Foreground(successColor).Bold(true).Render("●")
	case store.SessionIdle:
		return lipgloss.NewStyle().Foreground(warningColor).Render("○")
	case store.SessionError:
		return lipgloss.NewStyle().Foreground(dangerColor).Render("✗")
	case store.SessionFollowup:
		return lipgloss.NewStyle().Foreground(secondaryColor).Render("↻")
	default:
		return lipgloss.NewStyle().Foreground(dimColor).Render("·")
	}
}

func (m *SessionsModel) overlayMenu(base string) string {
	popup := m.menu.View()

	// Center the popup over the base content.
	popupLines := strings.Split(popup, "\n")
	baseLines := strings.Split(base, "\n")

	popupH := len(popupLines)
	popupW := 0
	for _, l := range popupLines {
		if w := lipgloss.Width(l); w > popupW {
			popupW = w
		}
	}

	// Place popup near the center of the screen.
	startRow := (m.height - popupH) / 2
	if startRow < 0 {
		startRow = 0
	}
	startCol := (m.width - popupW) / 2
	if startCol < 0 {
		startCol = 0
	}

	// Ensure baseLines has enough rows.
	for len(baseLines) < startRow+popupH {
		baseLines = append(baseLines, "")
	}

	// Overlay.
	for i, popLine := range popupLines {
		row := startRow + i
		if row >= len(baseLines) {
			break
		}
		baseLine := baseLines[row]
		// Pad base line to startCol.
		for lipgloss.Width(baseLine) < startCol {
			baseLine += " "
		}
		// Replace the region with popup content.
		baseLines[row] = baseLine[:startCol] + popLine
	}

	return strings.Join(baseLines, "\n")
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

// unreadSuffix returns ", N unread" if any rows are unread, or "" otherwise.
func unreadSuffix(rows []sessionsRow) string {
	n := 0
	for _, r := range rows {
		if r.unread {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d unread", n)
}
