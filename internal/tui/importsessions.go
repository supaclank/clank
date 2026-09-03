package tui

// importsessions.go — provider-selector modal for importing historical sessions.
//
// Shows a checklist of supported backends. The user
// toggles providers with space, then confirms with Enter. Esc cancels.
// On confirm, InboxModel runs discovery for the selected providers, then
// shows a summary confirmation using the existing confirmDialogModel.

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/supaclank/clank/internal/agent"
)

// importSessionsConfirmMsg is sent when the user confirms the provider selection.
type importSessionsConfirmMsg struct {
	providers []agent.BackendType // providers the user enabled
}

// importSessionsCancelMsg is sent when the user dismisses the modal.
type importSessionsCancelMsg struct{}

// importSessionsModel is a checklist modal for selecting providers to import
// sessions from.
type importSessionsModel struct {
	cursor  int
	checked map[agent.BackendType]bool
}

func newImportSessionsModel() importSessionsModel {
	// All providers are pre-checked for convenience.
	checked := make(map[agent.BackendType]bool, len(agent.AllBackends))
	for _, p := range agent.AllBackends {
		checked[p] = true
	}
	return importSessionsModel{
		cursor:  0,
		checked: checked,
	}
}

func (m importSessionsModel) Update(msg tea.Msg) (importSessionsModel, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		keyMsg = normalizeKeyCase(keyMsg)
		switch {
		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("esc", "q"))):
			return m, func() tea.Msg { return importSessionsCancelMsg{} }

		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("up", "k"))):
			if m.cursor > 0 {
				m.cursor--
			}
		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("down", "j"))):
			if m.cursor < len(agent.AllBackends)-1 {
				m.cursor++
			}
		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("space"))):
			p := agent.AllBackends[m.cursor]
			m.checked[p] = !m.checked[p]

		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("enter"))):
			var selected []agent.BackendType
			for _, p := range agent.AllBackends {
				if m.checked[p] {
					selected = append(selected, p)
				}
			}
			return m, func() tea.Msg {
				return importSessionsConfirmMsg{providers: selected}
			}
		}
	}
	return m, nil
}

func (m importSessionsModel) View() string {
	const innerWidth = 30
	var sb strings.Builder

	// Title.
	sb.WriteString(lipgloss.NewStyle().
		Bold(true).
		Foreground(primaryColor).
		Width(innerWidth).
		Render("Import Sessions"))
	sb.WriteString("\n\n")

	// Provider checklist.
	for i, p := range agent.AllBackends {
		check := "[ ]"
		if m.checked[p] {
			check = "[x]"
		}
		checkStyle := lipgloss.NewStyle().Foreground(dimColor)
		labelStyle := lipgloss.NewStyle().Foreground(textColor)

		if i == m.cursor {
			checkStyle = lipgloss.NewStyle().Foreground(primaryColor).Bold(true)
			labelStyle = lipgloss.NewStyle().Foreground(textColor).Bold(true)
		}

		prefix := "  "
		if i == m.cursor {
			prefix = lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		}
		line := prefix + checkStyle.Render(check) + " " + labelStyle.Render(backendDisplayName(p))
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// Hint.
	hint := lipgloss.NewStyle().
		Foreground(dimColor).
		Render("space: toggle  enter: import  esc: cancel")
	sb.WriteString(hint)

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())
}
