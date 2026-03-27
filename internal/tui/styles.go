package tui

import (
	"fmt"
	"image/color"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
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

// agentColor returns a distinct color for the given agent name so that modes
// are visually distinguishable at a glance.
func agentColor(name string) color.Color {
	switch name {
	case "build":
		return successColor // green – constructive/active
	case "plan":
		return secondaryColor // cyan – deliberate/reflective
	default:
		return dimColor
	}
}

// promptInputBorder is the external border+padding applied around the textarea.
// We render this outside the textarea because bubbles v2 has a bug where
// placeholderView() applies Base.Render() internally, and then View() applies
// it again — causing a double border when the textarea is empty.
var promptInputBorderSize = 2 + 2 // border (1+1) + padding (1+1)

func promptInputStyle(focused bool) lipgloss.Style {
	bc := mutedColor
	if focused {
		bc = primaryColor
	}
	return promptInputStyleWithColor(bc)
}

// promptInputStyleWithColor returns a prompt box style with the given border color.
func promptInputStyleWithColor(borderColor color.Color) lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1)
}

// newPromptTextarea creates a textarea configured for prompt input.
// Enter sends; shift+enter inserts a newline.
//
// The textarea itself has NO border — callers must wrap View() output with
// promptInputStyle(focused).Render(...) and pass (width - promptInputBorderSize)
// to SetWidth.
func newPromptTextarea(placeholder string, height int) textarea.Model {
	ta := textarea.New()
	ta.Placeholder = placeholder
	ta.CharLimit = 4096
	ta.SetHeight(height)
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	styles := ta.Styles()
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Focused.Base = lipgloss.NewStyle()
	styles.Blurred.Base = lipgloss.NewStyle()
	ta.SetStyles(styles)
	// Shift+Enter inserts newline; plain Enter is handled by the parent model.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter"),
		key.WithHelp("shift+enter", "newline"),
	)
	return ta
}

// timeAgo returns a human-readable relative time string.
func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dh ago", h)
	default:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd ago", days)
	}
}
