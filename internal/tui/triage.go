package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/acksell/clank/internal/analyzer"
	"github.com/acksell/clank/internal/store"
)

type triageModel struct {
	ticket   *store.Ticket
	analyzer *analyzer.Analyzer
	context  string
	result   string
	loading  bool
	err      error
	spinner  spinner.Model
	width    int
	height   int
}

type triageResultMsg struct {
	result string
	err    error
}

func newTriageModel(ticket *store.Ticket, a *analyzer.Analyzer, ctx string, width, height int) triageModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(primaryColor)

	return triageModel{
		ticket:   ticket,
		analyzer: a,
		context:  ctx,
		loading:  true,
		spinner:  s,
		width:    width,
		height:   height,
	}
}

func (m triageModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.runTriage())
}

func (m triageModel) runTriage() tea.Cmd {
	return func() tea.Msg {
		result, err := m.analyzer.TriageTicket(*m.ticket, m.context)
		return triageResultMsg{result: result, err: err}
	}
}

func (m triageModel) Update(msg tea.Msg) (triageModel, tea.Cmd) {
	switch msg := msg.(type) {
	case triageResultMsg:
		m.loading = false
		m.result = msg.result
		m.err = msg.err
		if msg.err == nil {
			m.ticket.AINotes = msg.result
		}
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			return m, func() tea.Msg { return backToDetailMsg{} }
		case "a":
			return m, func() tea.Msg {
				return ticketUpdatedMsg{ticket: *m.ticket}
			}
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}
	return m, nil
}

type backToDetailMsg struct{}

func (m triageModel) View() string {
	header := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render("CLANK  AI Triage")

	content := ""
	if m.loading {
		content = fmt.Sprintf("\n%s Analyzing ticket: %s\n\nThe AI is reviewing this ticket against your product context...",
			m.spinner.View(), m.ticket.Title)
	} else if m.err != nil {
		content = lipgloss.NewStyle().Foreground(dangerColor).Render(
			fmt.Sprintf("\nError: %v\n\nPress 'q' to go back.", m.err))
	} else {
		content = fmt.Sprintf("\n%s\n\n%s\n\n%s",
			subtitleStyle.Render("AI Analysis for: "+m.ticket.Title),
			aiNoteStyle.Render(m.result),
			helpStyle.Render("a: accept & save notes | q/esc: back without saving"),
		)
	}

	return fmt.Sprintf("%s\n%s", header, content)
}
