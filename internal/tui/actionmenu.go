package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// actionMenuItem represents a single item in the action menu.
type actionMenuItem struct {
	label     string
	key       string // short key hint shown on the right
	action    string // opaque action ID returned in actionMenuResultMsg
	separator bool   // if true, this is a visual separator (not selectable)
}

// actionMenuResultMsg is sent when the user selects an action.
type actionMenuResultMsg struct {
	action string
}

// actionMenuCancelMsg is sent when the user cancels the menu.
type actionMenuCancelMsg struct{}

type actionMenuModel struct {
	title   string
	items   []actionMenuItem
	cursor  int
	width   int
	visible bool
}

func newActionMenu(title string, items []actionMenuItem) actionMenuModel {
	m := actionMenuModel{
		title: title,
		items: items,
	}
	// Start cursor on the first selectable item.
	for i, item := range items {
		if !item.separator {
			m.cursor = i
			break
		}
	}
	return m
}

func (m actionMenuModel) Init() tea.Cmd {
	return nil
}

func (m actionMenuModel) Update(msg tea.Msg) (actionMenuModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q"))):
			return m, func() tea.Msg { return actionMenuCancelMsg{} }
		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
			m.moveCursor(-1)
		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
			m.moveCursor(1)
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			if m.cursor >= 0 && m.cursor < len(m.items) && !m.items[m.cursor].separator {
				action := m.items[m.cursor].action
				return m, func() tea.Msg { return actionMenuResultMsg{action: action} }
			}
		default:
			// Check for shortcut keys.
			k := msg.String()
			for _, item := range m.items {
				if !item.separator && item.key == k {
					action := item.action
					return m, func() tea.Msg { return actionMenuResultMsg{action: action} }
				}
			}
		}
	}
	return m, nil
}

func (m *actionMenuModel) moveCursor(dir int) {
	for {
		m.cursor += dir
		if m.cursor < 0 {
			m.cursor = 0
			return
		}
		if m.cursor >= len(m.items) {
			m.cursor = len(m.items) - 1
			return
		}
		if !m.items[m.cursor].separator {
			return
		}
	}
}

func (m actionMenuModel) View() string {
	var sb strings.Builder

	menuWidth := 36
	if len(m.title) > menuWidth-4 {
		menuWidth = len(m.title) + 4
	}
	innerWidth := menuWidth - 4

	// Title
	titleStr := lipgloss.NewStyle().
		Bold(true).
		Foreground(textColor).
		Width(innerWidth).
		Render(m.title)
	sb.WriteString(titleStr)
	sb.WriteString("\n")

	for i, item := range m.items {
		if item.separator {
			sep := lipgloss.NewStyle().
				Foreground(mutedColor).
				Width(innerWidth).
				Render(strings.Repeat("─", innerWidth))
			sb.WriteString(sep)
			sb.WriteString("\n")
			continue
		}

		label := item.label
		if item.key != "" {
			padding := innerWidth - lipgloss.Width(label) - lipgloss.Width(item.key)
			if padding < 1 {
				padding = 1
			}
			label = label + strings.Repeat(" ", padding) + lipgloss.NewStyle().Foreground(dimColor).Render(item.key)
		}

		if i == m.cursor {
			line := lipgloss.NewStyle().
				Background(primaryColor).
				Foreground(textColor).
				Bold(true).
				Width(innerWidth).
				Render(label)
			sb.WriteString(line)
		} else {
			line := lipgloss.NewStyle().
				Foreground(textColor).
				Width(innerWidth).
				Render(label)
			sb.WriteString(line)
		}
		sb.WriteString("\n")
	}

	// Hint line
	hint := lipgloss.NewStyle().
		Foreground(dimColor).
		Render("↑↓ navigate  enter select  esc cancel")
	sb.WriteString(hint)

	popup := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())

	return popup
}
