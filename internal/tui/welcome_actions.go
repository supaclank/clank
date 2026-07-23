package tui

import tea "charm.land/bubbletea/v2"

type welcomeAction int

const (
	welcomeActionNewSession welcomeAction = iota
	welcomeActionNewWorktreeSession
	welcomeActionImportSessions
	welcomeActionSettings
	welcomeActionCount
)

type welcomeActionItem struct {
	Title       string
	Description string
}

var welcomeActionItems = [...]welcomeActionItem{
	{Title: "New session", Description: "Start an agent in this worktree"},
	{Title: "New worktree session", Description: "Start from a fresh branch"},
	{Title: "Import sessions", Description: "Bring in work from local providers"},
	{Title: "Settings", Description: "Configure your agent backend"},
}

func (m *InboxModel) moveWelcomeCursor(delta int) {
	next := int(m.welcomeCursor) + delta
	if next < int(welcomeActionNewSession) {
		next = int(welcomeActionNewSession)
	}
	if next >= int(welcomeActionCount) {
		next = int(welcomeActionCount) - 1
	}
	m.welcomeCursor = welcomeAction(next)
}

func (m *InboxModel) activateWelcomeAction() tea.Cmd {
	switch m.welcomeCursor {
	case welcomeActionNewSession:
		return m.openComposingSession("", false)
	case welcomeActionNewWorktreeSession:
		return m.openComposingSession("", true)
	case welcomeActionImportSessions:
		m.showImportSessions = true
		m.importSessions = newImportSessionsModel()
	case welcomeActionSettings:
		m.openSettings()
	}
	return nil
}
