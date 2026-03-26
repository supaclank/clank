package tui

import (
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/store"
)

var (
	primaryColor   = lipgloss.Color("#7C3AED")
	secondaryColor = lipgloss.Color("#06B6D4")
	successColor   = lipgloss.Color("#10B981")
	warningColor   = lipgloss.Color("#F59E0B")
	dangerColor    = lipgloss.Color("#EF4444")
	mutedColor     = lipgloss.Color("#6B7280")
	textColor      = lipgloss.Color("#F9FAFB")
	dimColor       = lipgloss.Color("#9CA3AF")

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(primaryColor).
			MarginBottom(1)

	subtitleStyle = lipgloss.NewStyle().
			Foreground(secondaryColor).
			Bold(true)

	labelStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	valueStyle = lipgloss.NewStyle().
			Foreground(textColor)

	selectedStyle = lipgloss.NewStyle().
			Background(primaryColor).
			Foreground(textColor).
			Bold(true).
			Padding(0, 1)

	statusStyles = map[string]lipgloss.Style{
		"new":       lipgloss.NewStyle().Foreground(secondaryColor).Bold(true),
		"triaged":   lipgloss.NewStyle().Foreground(primaryColor),
		"backlog":   lipgloss.NewStyle().Foreground(warningColor),
		"doing":     lipgloss.NewStyle().Foreground(successColor).Bold(true),
		"done":      lipgloss.NewStyle().Foreground(successColor),
		"discarded": lipgloss.NewStyle().Foreground(mutedColor).Strikethrough(true),
	}

	typeStyles = map[string]lipgloss.Style{
		"unfinished_thread": lipgloss.NewStyle().Foreground(warningColor),
		"opportunity":       lipgloss.NewStyle().Foreground(secondaryColor),
	}

	helpStyle = lipgloss.NewStyle().
			Foreground(dimColor).
			MarginTop(1)

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(primaryColor).
			Padding(1, 2)

	aiNoteStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(secondaryColor).
			Padding(0, 1).
			MarginTop(1)
)

func styledStatus(status string) string {
	if s, ok := statusStyles[status]; ok {
		return s.Render(status)
	}
	return status
}

func styledType(t string) string {
	if s, ok := typeStyles[t]; ok {
		return s.Render(t)
	}
	return t
}

func styledComplexity(c int) string {
	s := ""
	for i := 0; i < 10; i++ {
		if i < c {
			s += lipgloss.NewStyle().Foreground(warningColor).Render("*")
		} else {
			s += lipgloss.NewStyle().Foreground(mutedColor).Render(".")
		}
	}
	return s
}

func styledImpact(i int) string {
	s := ""
	for j := 0; j < 10; j++ {
		if j < i {
			s += lipgloss.NewStyle().Foreground(secondaryColor).Render("*")
		} else {
			s += lipgloss.NewStyle().Foreground(mutedColor).Render(".")
		}
	}
	return s
}

func styledQuadrant(q store.Quadrant) string {
	switch q {
	case store.QuadrantQuickWin:
		return lipgloss.NewStyle().Foreground(successColor).Bold(true).Render("quickwin")
	case store.QuadrantValueBet:
		return lipgloss.NewStyle().Foreground(dangerColor).Bold(true).Render("valuebet")
	case store.QuadrantDistraction:
		return lipgloss.NewStyle().Foreground(mutedColor).Render("distraction")
	case store.QuadrantTidyUp:
		return lipgloss.NewStyle().Foreground(secondaryColor).Render("tidyup")
	default:
		return lipgloss.NewStyle().Foreground(dimColor).Render("—")
	}
}
