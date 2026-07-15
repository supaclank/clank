package tui

// Built-in color schemes for the TUI. The first entry (index 0) is the
// default scheme and is used as the fallback when preferences don't
// specify a scheme or specify an unknown one.
//
// All hex values are expressed as `#RRGGBB` strings; they're converted
// to lipgloss.Color lazily inside applyColorScheme.

var builtInSchemes = []ColorScheme{
	{
		Name:      "gruvbox-dark",
		Primary:   "#D3869B", // purple
		Secondary: "#83A598", // aqua
		Success:   "#B8BB26", // green
		Warning:   "#FABD2F", // yellow
		Danger:    "#FB4934", // red
		Muted:     "#7C6F64", // gray
		Text:      "#EBDBB2", // fg
		Dim:       "#A89984", // gray-light
		Draft:     "#FE8019", // orange
	},
	{
		Name:      "tokyo-night",
		Primary:   "#BB9AF7",
		Secondary: "#7AA2F7",
		Success:   "#9ECE6A",
		Warning:   "#E0AF68",
		Danger:    "#F7768E",
		Muted:     "#565F89",
		Text:      "#C0CAF5",
		Dim:       "#737AA2",
		Draft:     "#FF9E64",
	},
	{
		Name:      "catppuccin-mocha",
		Primary:   "#CBA6F7", // mauve
		Secondary: "#89DCEB", // sky
		Success:   "#A6E3A1", // green
		Warning:   "#F9E2AF", // yellow
		Danger:    "#F38BA8", // red
		Muted:     "#6C7086", // overlay0
		Text:      "#CDD6F4", // text
		Dim:       "#A6ADC8", // subtext0
		Draft:     "#F5C2E7", // pink
	},
	{
		Name:      "dracula",
		Primary:   "#BD93F9", // purple
		Secondary: "#8BE9FD", // cyan
		Success:   "#50FA7B", // green
		Warning:   "#F1FA8C", // yellow
		Danger:    "#FF5555", // red
		Muted:     "#6272A4", // comment
		Text:      "#F8F8F2", // foreground
		Dim:       "#BFBFBF",
		Draft:     "#FF79C6", // pink
	},
	{
		Name:      "violet",
		Primary:   "#7C3AED", // violet-600
		Secondary: "#06B6D4", // cyan-500
		Success:   "#10B981", // emerald-500
		Warning:   "#F59E0B", // amber-500
		Danger:    "#EF4444", // red-500
		Muted:     "#6B7280", // gray-500
		Text:      "#F9FAFB", // gray-50
		Dim:       "#9CA3AF", // gray-400
		Draft:     "#EC4899", // pink-500
	},
}

// schemeNames returns the names of all built-in schemes in display order.
func schemeNames() []string {
	names := make([]string, len(builtInSchemes))
	for i, s := range builtInSchemes {
		names[i] = s.Name
	}
	return names
}
