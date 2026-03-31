package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// confirmResultMsg is sent when the user confirms or cancels the dialog.
type confirmResultMsg struct {
	confirmed bool
	action    string // opaque action ID passed through from the caller
}

// confirmDialogModel is a simple yes/no confirmation popup.
type confirmDialogModel struct {
	title   string
	message string
	action  string // opaque action ID returned in confirmResultMsg
	cursor  int    // 0 = yes, 1 = no
}

func newConfirmDialog(title, message, action string) confirmDialogModel {
	return confirmDialogModel{
		title:   title,
		message: message,
		action:  action,
		cursor:  1, // default to "No" (safe choice)
	}
}

func (m confirmDialogModel) Update(msg tea.Msg) (confirmDialogModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		msg = normalizeKeyCase(msg)
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q", "n"))):
			return m, func() tea.Msg {
				return confirmResultMsg{confirmed: false, action: m.action}
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("y"))):
			return m, func() tea.Msg {
				return confirmResultMsg{confirmed: true, action: m.action}
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("left", "h"))):
			m.cursor = 0
		case key.Matches(msg, key.NewBinding(key.WithKeys("right", "l"))):
			m.cursor = 1
		case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
			m.cursor = (m.cursor + 1) % 2
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			return m, func() tea.Msg {
				return confirmResultMsg{confirmed: m.cursor == 0, action: m.action}
			}
		}
	}
	return m, nil
}

func (m confirmDialogModel) View() string {
	var sb strings.Builder

	menuWidth := 36
	if len(m.title) > menuWidth-4 {
		menuWidth = len(m.title) + 4
	}
	innerWidth := menuWidth - 4

	// Title.
	titleStr := lipgloss.NewStyle().
		Bold(true).
		Foreground(warningColor).
		Width(innerWidth).
		Render(m.title)
	sb.WriteString(titleStr)
	sb.WriteString("\n\n")

	// Message.
	msgStr := lipgloss.NewStyle().
		Foreground(textColor).
		Width(innerWidth).
		Render(m.message)
	sb.WriteString(msgStr)
	sb.WriteString("\n\n")

	// Buttons: [Yes]  [No]
	yesStyle := lipgloss.NewStyle().Foreground(textColor).Padding(0, 2)
	noStyle := lipgloss.NewStyle().Foreground(textColor).Padding(0, 2)
	if m.cursor == 0 {
		yesStyle = yesStyle.Background(dangerColor).Bold(true)
	}
	if m.cursor == 1 {
		noStyle = noStyle.Background(primaryColor).Bold(true)
	}

	buttons := yesStyle.Render("Yes") + "  " + noStyle.Render("No")
	sb.WriteString(buttons)
	sb.WriteString("\n\n")

	// Hint.
	hint := lipgloss.NewStyle().
		Foreground(dimColor).
		Render("y: yes  n/esc: no  tab: switch  enter: confirm")
	sb.WriteString(hint)

	popup := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(warningColor).
		Padding(1, 2).
		Render(sb.String())

	return popup
}
