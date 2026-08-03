package tui

import (
	"charm.land/lipgloss/v2"

	"github.com/supaclank/clank/internal/agent"
)

// styledAgentStatus returns the styled glyph that represents a session's
// agent status. For busy/starting states the caller supplies the current
// spinner frame so animation stays in sync with the model that owns the
// spinner.
func styledAgentStatus(status agent.SessionStatus, spinnerView string) string {
	switch status {
	case agent.StatusBusy, agent.StatusStarting:
		return spinnerView
	case agent.StatusIdle:
		return lipgloss.NewStyle().Foreground(warningColor).Render("○")
	case agent.StatusError:
		return lipgloss.NewStyle().Foreground(dangerColor).Render("✗")
	case agent.StatusDead:
		return lipgloss.NewStyle().Foreground(mutedColor).Render("·")
	default:
		return lipgloss.NewStyle().Foreground(dimColor).Render("·")
	}
}
