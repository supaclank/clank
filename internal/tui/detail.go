package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/acksell/clank/internal/store"
)

type detailModel struct {
	ticket   *store.Ticket
	viewport viewport.Model
	width    int
	height   int

	editing    bool
	editField  int
	editFields []editableField
}

type editableField struct {
	name    string
	value   string
	options []string
}

func newDetailModel(ticket *store.Ticket, width, height int) detailModel {
	m := detailModel{
		ticket: ticket,
		width:  width,
		height: height,
	}
	m.buildEditFields()
	m.updateViewport()
	return m
}

func (m *detailModel) buildEditFields() {
	m.editFields = []editableField{
		{name: "status", value: string(m.ticket.Status), options: []string{"new", "triaged", "backlog", "doing", "done", "discarded"}},
		{name: "title", value: m.ticket.Title},
		{name: "summary", value: m.ticket.Summary},
		{name: "complexity", value: fmt.Sprintf("%d", m.ticket.Complexity), options: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}},
		{name: "impact", value: fmt.Sprintf("%d", m.ticket.Impact), options: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}},
		{name: "labels", value: strings.Join(m.ticket.Labels, ", ")},
		{name: "user_notes", value: m.ticket.UserNotes},
	}
}

func (m *detailModel) updateViewport() {
	content := m.renderContent()
	vp := viewport.New(m.width, m.height-4)
	vp.SetContent(content)
	m.viewport = vp
}

func (m detailModel) renderContent() string {
	t := m.ticket
	var sb strings.Builder

	sb.WriteString(titleStyle.Render(t.Title))
	sb.WriteString("\n\n")

	grid := []struct{ label, value string }{
		{"Type", styledType(string(t.Type))},
		{"Status", styledStatus(string(t.Status))},
		{"Complexity", styledComplexity(t.Complexity)},
		{"Impact", styledImpact(t.Impact)},
		{"Quadrant", styledQuadrant(t.Quadrant())},
		{"Repo", t.RepoPath},
		{"Session", t.SessionTitle},
		{"Date", t.SessionDate.Format("2006-01-02 15:04")},
		{"ID", t.ID},
	}
	for _, g := range grid {
		sb.WriteString(labelStyle.Render(fmt.Sprintf("%-12s", g.label)))
		sb.WriteString(valueStyle.Render(g.value))
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("Summary"))
	sb.WriteString("\n")
	sb.WriteString(t.Summary)
	sb.WriteString("\n\n")

	if t.Description != "" {
		sb.WriteString(subtitleStyle.Render("Description"))
		sb.WriteString("\n")
		sb.WriteString(t.Description)
		sb.WriteString("\n\n")
	}

	if len(t.SourceQuotes) > 0 {
		sb.WriteString(subtitleStyle.Render("Source Quotes"))
		sb.WriteString("\n")
		for _, q := range t.SourceQuotes {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Italic(true).Render("  > " + q))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	if t.AINotes != "" {
		sb.WriteString(subtitleStyle.Render("AI Notes"))
		sb.WriteString("\n")
		sb.WriteString(aiNoteStyle.Render(t.AINotes))
		sb.WriteString("\n\n")
	}

	if t.UserNotes != "" {
		sb.WriteString(subtitleStyle.Render("Your Notes"))
		sb.WriteString("\n")
		sb.WriteString(t.UserNotes)
		sb.WriteString("\n")
	}

	if m.editing {
		sb.WriteString("\n\n")
		sb.WriteString(subtitleStyle.Render("-- Edit Mode --"))
		sb.WriteString("\n")
		for i, f := range m.editFields {
			prefix := "  "
			if i == m.editField {
				prefix = selectedStyle.Render(">") + " "
			}
			val := f.value
			if val == "" {
				val = lipgloss.NewStyle().Foreground(mutedColor).Render("(empty)")
			}
			sb.WriteString(fmt.Sprintf("%s%s: %s\n", prefix, labelStyle.Render(f.name), val))
		}
	}

	return sb.String()
}

func (m detailModel) Init() tea.Cmd {
	return nil
}

type ticketUpdatedMsg struct {
	ticket store.Ticket
}

func (m detailModel) Update(msg tea.Msg) (detailModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.editing {
			return m.updateEditing(msg)
		}
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("e"))):
			m.editing = true
			m.editField = 0
			m.updateViewport()
		case key.Matches(msg, key.NewBinding(key.WithKeys("q", "esc"))):
			return m, func() tea.Msg { return backToListMsg{} }
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateViewport()
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m detailModel) updateEditing(msg tea.KeyMsg) (detailModel, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.editing = false
		m.updateViewport()
	case "up", "k":
		if m.editField > 0 {
			m.editField--
			m.updateViewport()
		}
	case "down", "j":
		if m.editField < len(m.editFields)-1 {
			m.editField++
			m.updateViewport()
		}
	case "enter", " ", "tab":
		f := &m.editFields[m.editField]
		if f.options != nil {
			for i, opt := range f.options {
				if opt == f.value {
					f.value = f.options[(i+1)%len(f.options)]
					break
				}
			}
		}
		m.applyEdits()
		m.updateViewport()
	case "backspace":
		f := &m.editFields[m.editField]
		if f.options == nil && len(f.value) > 0 {
			f.value = f.value[:len(f.value)-1]
			m.applyEdits()
			m.updateViewport()
		}
	default:
		f := &m.editFields[m.editField]
		if f.options == nil && len(msg.String()) == 1 {
			f.value += msg.String()
			m.applyEdits()
			m.updateViewport()
		}
	}
	return m, nil
}

func (m *detailModel) applyEdits() {
	for _, f := range m.editFields {
		switch f.name {
		case "status":
			m.ticket.Status = store.TicketStatus(f.value)
		case "title":
			m.ticket.Title = f.value
		case "summary":
			m.ticket.Summary = f.value
		case "complexity":
			c := 0
			fmt.Sscanf(f.value, "%d", &c)
			m.ticket.Complexity = c
		case "impact":
			i := 0
			fmt.Sscanf(f.value, "%d", &i)
			m.ticket.Impact = i
		case "labels":
			labels := strings.Split(f.value, ",")
			m.ticket.Labels = nil
			for _, l := range labels {
				l = strings.TrimSpace(l)
				if l != "" {
					m.ticket.Labels = append(m.ticket.Labels, l)
				}
			}
		case "user_notes":
			m.ticket.UserNotes = f.value
		}
	}
}

func (m detailModel) View() string {
	header := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render("CLANK  Ticket Detail")

	help := helpStyle.Render("e: edit | up/down: scroll | q/esc: back")
	if m.editing {
		help = helpStyle.Render("up/down: navigate | enter/space: toggle | esc: stop editing")
	}

	return fmt.Sprintf("%s\n%s\n%s", header, m.viewport.View(), help)
}
