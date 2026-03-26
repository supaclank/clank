package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/store"
)

type listModel struct {
	table   table.Model
	tickets []store.Ticket
	width   int
	height  int
}

func newListModel(tickets []store.Ticket, width, height int) listModel {
	m := listModel{
		tickets: tickets,
		width:   width,
		height:  height,
	}
	m.buildTable()
	return m
}

func (m *listModel) buildTable() {
	titleWidth := m.width - 80
	if titleWidth < 20 {
		titleWidth = 20
	}

	columns := []table.Column{
		{Title: "Status", Width: 10},
		{Title: "Type", Width: 10},
		{Title: "Title", Width: titleWidth},
		{Title: "Repo", Width: 18},
		{Title: "C", Width: 3},
		{Title: "I", Width: 3},
		{Title: "Quadrant", Width: 11},
		{Title: "Labels", Width: 18},
	}

	rows := make([]table.Row, len(m.tickets))
	for i, t := range m.tickets {
		repoName := filepath.Base(t.RepoPath)
		labels := strings.Join(t.Labels, ",")
		if len(labels) > 18 {
			labels = labels[:15] + "..."
		}
		q := string(t.Quadrant())
		if q == "" {
			q = "—"
		}
		rows[i] = table.Row{
			string(t.Status),
			shortType(string(t.Type)),
			truncate(t.Title, titleWidth),
			repoName,
			fmt.Sprintf("%d", t.Complexity),
			fmt.Sprintf("%d", t.Impact),
			q,
			labels,
		}
	}

	h := m.height - 6
	if h < 5 {
		h = 5
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(h),
	)

	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(primaryColor).
		BorderBottom(true).
		Bold(true).
		Foreground(primaryColor)
	s.Selected = s.Selected.
		Foreground(textColor).
		Background(primaryColor).
		Bold(true)
	t.SetStyles(s)

	m.table = t
}

func (m listModel) Init() tea.Cmd {
	return nil
}

type selectTicketMsg struct {
	index int
}

type backToListMsg struct{}

func (m listModel) Update(msg tea.Msg) (listModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			idx := m.table.Cursor()
			if idx >= 0 && idx < len(m.tickets) {
				return m, func() tea.Msg { return selectTicketMsg{index: idx} }
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("d"))):
			idx := m.table.Cursor()
			if idx >= 0 && idx < len(m.tickets) {
				t := &m.tickets[idx]
				t.Status = nextStatus(t.Status)
				m.buildTable()
				return m, func() tea.Msg { return ticketUpdatedMsg{ticket: *t} }
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("x"))):
			idx := m.table.Cursor()
			if idx >= 0 && idx < len(m.tickets) {
				t := &m.tickets[idx]
				t.Status = store.StatusDiscarded
				m.buildTable()
				return m, func() tea.Msg { return ticketUpdatedMsg{ticket: *t} }
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("b"))):
			idx := m.table.Cursor()
			if idx >= 0 && idx < len(m.tickets) {
				t := &m.tickets[idx]
				t.Status = store.StatusBacklog
				m.buildTable()
				return m, func() tea.Msg { return ticketUpdatedMsg{ticket: *t} }
			}
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.buildTable()
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m listModel) View() string {
	header := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render(fmt.Sprintf("CLANK  %d tickets", len(m.tickets)))

	help := helpStyle.Render("enter: detail | b: backlog | x: discard | d: cycle status | a: AI triage | q: quit")

	return fmt.Sprintf("%s\n%s\n%s", header, m.table.View(), help)
}

func nextStatus(s store.TicketStatus) store.TicketStatus {
	order := []store.TicketStatus{
		store.StatusNew, store.StatusTriaged, store.StatusBacklog,
		store.StatusDoing, store.StatusDone, store.StatusDiscarded,
	}
	for i, st := range order {
		if st == s {
			return order[(i+1)%len(order)]
		}
	}
	return store.StatusNew
}

func shortType(t string) string {
	switch t {
	case "unfinished_thread":
		return "thread"
	case "opportunity":
		return "oppty"
	default:
		return t
	}
}

func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 20
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
