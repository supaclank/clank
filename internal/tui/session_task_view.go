package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m *SessionTaskModel) View() tea.View {
	width := m.session.width
	if width < 20 {
		width = 80
	}
	m.session.width = width

	status := "starting"
	if m.session.info != nil {
		status = string(m.session.info.Status)
	}
	headerStyle := lipgloss.NewStyle().Foreground(primaryColor).Bold(true)
	statusStyle := lipgloss.NewStyle().Foreground(dimColor)
	header := headerStyle.Render(m.options.Title) + " " + statusStyle.Render("["+status+"]")

	lines := m.taskContentLines()
	if len(lines) > m.options.MaxVisibleLines {
		lines = lines[len(lines)-m.options.MaxVisibleLines:]
	}

	var sb strings.Builder
	sb.WriteString(header)
	sb.WriteString("\n")
	for _, line := range lines {
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	if len(m.session.pendingPerms) == 0 {
		footer := "Non-interactive setup · ctrl+c to cancel"
		if m.result.Status != "" || m.result.Err != nil {
			footer = "Agent finished · validating generated configuration…"
		}
		sb.WriteString(helpStyle.Render(footer))
	}

	view := tea.NewView(strings.TrimRight(sb.String(), "\n"))
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	return view
}

func (m *SessionTaskModel) taskContentLines() []string {
	entries := make([]displayEntry, 0, len(m.session.entries))
	for _, entry := range m.session.entries {
		if entry.kind != entryUser {
			entries = append(entries, entry)
		}
	}

	originalEntries := m.session.entries
	originalCursor := m.session.cursor
	originalStarts := m.session.entryStartLine
	originalEnds := m.session.entryEndLine
	originalCount := m.session.lastContentLineCount
	m.session.entries = entries
	m.session.cursor = -1
	lines := m.session.buildContentLines()
	m.session.entries = originalEntries
	m.session.cursor = originalCursor
	m.session.entryStartLine = originalStarts
	m.session.entryEndLine = originalEnds
	m.session.lastContentLineCount = originalCount
	return lines
}
