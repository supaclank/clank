package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/acksell/clank/internal/agent"
)

// newSessionLaunchMsg is emitted when the user launches a new session.
type newSessionLaunchMsg struct {
	req agent.StartRequest
}

// newSessionCancelMsg is emitted when the user cancels the dialog.
type newSessionCancelMsg struct{}

// newSessionField identifies which field is focused in the dialog.
type newSessionField int

const (
	fieldBackend newSessionField = iota
	fieldProject
	fieldPrompt
)

const numFields = 3

// newSessionModel is the new session dialog form.
type newSessionModel struct {
	backend    agent.BackendType
	projectDir string
	prompt     textarea.Model
	focus      newSessionField
	width      int
	height     int
	err        error
}

func newNewSessionModel(projectDir string) newSessionModel {
	ta := textarea.New()
	ta.Placeholder = "Describe the task for the agent..."
	ta.CharLimit = 4096
	ta.SetHeight(5)
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()

	m := newSessionModel{
		backend:    agent.BackendOpenCode,
		projectDir: projectDir,
		prompt:     ta,
		focus:      fieldPrompt, // start on prompt since it's the main input
	}
	m.prompt.Focus()
	return m
}

func (m newSessionModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m newSessionModel) Update(msg tea.Msg) (newSessionModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		promptWidth := m.width - 8
		if promptWidth < 20 {
			promptWidth = 20
		}
		m.prompt.SetWidth(promptWidth)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Forward to textarea when focused on prompt.
	if m.focus == fieldPrompt {
		var cmd tea.Cmd
		m.prompt, cmd = m.prompt.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m newSessionModel) handleKey(msg tea.KeyMsg) (newSessionModel, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		return m, tea.Quit

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		return m, func() tea.Msg { return newSessionCancelMsg{} }

	case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
		m.focus = (m.focus + 1) % numFields
		m.updateFocus()
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))):
		m.focus = (m.focus - 1 + numFields) % numFields
		m.updateFocus()
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+l"))):
		// Launch shortcut.
		return m.launch()

	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		// Enter on backend/project fields moves to next field.
		// Enter in prompt inserts a newline (handled by textarea).
		if m.focus != fieldPrompt {
			m.focus++
			m.updateFocus()
			return m, nil
		}
		// Alt+Enter or Ctrl+Enter in prompt to launch? No — just use Ctrl+L.
		// Let textarea handle enter for newlines.
		var cmd tea.Cmd
		m.prompt, cmd = m.prompt.Update(msg)
		return m, cmd
	}

	// Backend field: left/right or h/l to toggle.
	if m.focus == fieldBackend {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("left", "right", "h", "l", " "))):
			if m.backend == agent.BackendOpenCode {
				m.backend = agent.BackendClaudeCode
			} else {
				m.backend = agent.BackendOpenCode
			}
			return m, nil
		}
	}

	// Forward to textarea when focused on prompt.
	if m.focus == fieldPrompt {
		var cmd tea.Cmd
		m.prompt, cmd = m.prompt.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *newSessionModel) updateFocus() {
	if m.focus == fieldPrompt {
		m.prompt.Focus()
	} else {
		m.prompt.Blur()
	}
}

func (m newSessionModel) launch() (newSessionModel, tea.Cmd) {
	prompt := strings.TrimSpace(m.prompt.Value())
	if prompt == "" {
		m.err = fmt.Errorf("prompt is required")
		return m, nil
	}
	if m.projectDir == "" {
		m.err = fmt.Errorf("project directory is required")
		return m, nil
	}
	m.err = nil
	req := agent.StartRequest{
		Backend:    m.backend,
		ProjectDir: m.projectDir,
		Prompt:     prompt,
	}
	return m, func() tea.Msg { return newSessionLaunchMsg{req: req} }
}

func (m newSessionModel) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	var sb strings.Builder

	// Title.
	title := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render("New Session")
	sb.WriteString(title)
	sb.WriteString("\n\n")

	// Error.
	if m.err != nil {
		errMsg := lipgloss.NewStyle().Foreground(dangerColor).Render(fmt.Sprintf("Error: %v", m.err))
		sb.WriteString(errMsg)
		sb.WriteString("\n\n")
	}

	labelSty := lipgloss.NewStyle().Foreground(dimColor).Width(12)
	focusSty := lipgloss.NewStyle().Foreground(primaryColor).Bold(true)

	// Backend selector.
	backendLabel := labelSty.Render("Backend:")
	ocStyle := lipgloss.NewStyle().Foreground(dimColor)
	ccStyle := lipgloss.NewStyle().Foreground(dimColor)
	if m.backend == agent.BackendOpenCode {
		ocStyle = lipgloss.NewStyle().Foreground(successColor).Bold(true)
	} else {
		ccStyle = lipgloss.NewStyle().Foreground(successColor).Bold(true)
	}
	backendValue := fmt.Sprintf("[%s]  [%s]",
		ocStyle.Render("OpenCode"),
		ccStyle.Render("Claude Code"),
	)
	indicator := "  "
	if m.focus == fieldBackend {
		indicator = focusSty.Render("> ")
	}
	sb.WriteString(indicator + backendLabel + backendValue)
	sb.WriteString("\n")

	// Project.
	projectLabel := labelSty.Render("Project:")
	projectValue := lipgloss.NewStyle().Foreground(textColor).Render(m.projectDir)
	indicator = "  "
	if m.focus == fieldProject {
		indicator = focusSty.Render("> ")
	}
	sb.WriteString(indicator + projectLabel + projectValue)
	sb.WriteString("\n\n")

	// Prompt.
	promptLabel := "  " + labelSty.Render("Prompt:")
	if m.focus == fieldPrompt {
		promptLabel = focusSty.Render("> ") + labelSty.Render("Prompt:")
	}
	sb.WriteString(promptLabel)
	sb.WriteString("\n")

	promptBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(dimColor).
		Padding(0, 1).
		MarginLeft(2)
	if m.focus == fieldPrompt {
		promptBox = promptBox.BorderForeground(primaryColor)
	}
	sb.WriteString(promptBox.Render(m.prompt.View()))
	sb.WriteString("\n\n")

	// Help.
	help := helpStyle.Render("tab: next field | ←/→: toggle backend | ctrl+l: launch | esc: cancel")
	sb.WriteString(help)

	return sb.String()
}
