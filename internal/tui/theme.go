package tui

// Color scheme runtime swapping.
//
// The TUI defines nine semantic palette colors as package-level `var`s in
// styles.go (primaryColor, secondaryColor, etc.). Most call sites read
// those vars at render time via `lipgloss.NewStyle().Foreground(primaryColor)`,
// so a reassignment is picked up automatically on the next frame.
//
// A handful of module-level styles (titleStyle, selectedStyle, borderStyle,
// etc.) are built once at package-init time — they capture the color values
// at that moment. applyColorScheme rebuilds those so a scheme swap takes
// full effect without a TUI restart.

import (
	"charm.land/lipgloss/v2"
)

// ColorScheme is a named palette. All color fields are hex strings
// (e.g. "#7C3AED") so the struct is trivially JSON-serializable and
// easy to read/edit.
type ColorScheme struct {
	Name      string `json:"name"`
	Primary   string `json:"primary"`
	Secondary string `json:"secondary"`
	Success   string `json:"success"`
	Warning   string `json:"warning"`
	Danger    string `json:"danger"`
	Muted     string `json:"muted"`
	Text      string `json:"text"`
	Dim       string `json:"dim"`
	Draft     string `json:"draft"`
}

// applyColorScheme mutates the package-level palette vars and rebuilds
// the captured module-level styles. Safe to call from the TUI goroutine;
// MUST NOT be called concurrently with View() renders (bubbletea already
// serializes Update/View, so normal callers are fine).
func applyColorScheme(s ColorScheme) {
	primaryColor = lipgloss.Color(s.Primary)
	secondaryColor = lipgloss.Color(s.Secondary)
	successColor = lipgloss.Color(s.Success)
	warningColor = lipgloss.Color(s.Warning)
	dangerColor = lipgloss.Color(s.Danger)
	mutedColor = lipgloss.Color(s.Muted)
	textColor = lipgloss.Color(s.Text)
	dimColor = lipgloss.Color(s.Dim)
	draftColor = lipgloss.Color(s.Draft)

	rebuildModuleStyles()
}

// rebuildModuleStyles re-constructs the module-level lipgloss.Style values
// in styles.go that capture palette colors at init time. Keep this in sync
// with the `var (...)` block in styles.go.
func rebuildModuleStyles() {
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
}

// schemeByName looks up a built-in scheme by name. Returns false for
// unknown names. An empty name resolves to the default scheme.
func schemeByName(name string) (ColorScheme, bool) {
	if name == "" {
		return builtInSchemes[0], true // default is first
	}
	for _, s := range builtInSchemes {
		if s.Name == name {
			return s, true
		}
	}
	return ColorScheme{}, false
}

// applySchemeFromPreference resolves `name` to a built-in scheme and
// applies it. Unknown non-empty names silently fall back to the default
// (we don't want a corrupt preferences file to brick the TUI).
func applySchemeFromPreference(name string) {
	s, ok := schemeByName(name)
	if !ok {
		s = builtInSchemes[0]
	}
	applyColorScheme(s)
}
