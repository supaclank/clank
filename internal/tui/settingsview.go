package tui

// settingsview.go — the Settings "page" rendered in the right pane of the
// inbox layout. It's a simple cursor-navigable list of setting entries.
// Activating an entry opens the corresponding editor (for now only a color
// scheme picker overlay).
//
// Navigation contract:
//
//	up/down / j/k   → move cursor between entries
//	enter           → activate the entry under the cursor
//	left            → return focus to the sidebar (handled by inbox)
//	esc             → close the settings page (handled by inbox)
//
// The page is intentionally lightweight — it holds no persistent state
// and re-derives its entry list on each construction. This keeps the
// door open for dynamic entries later (feature flags, plugin settings).

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// settingsEntryKind identifies which editor to open for a given row.
type settingsEntryKind int

const (
	settingsEntryColorScheme settingsEntryKind = iota
	settingsEntryDefaultBackend
	settingsEntryActiveHub
)

// settingsEntry is one row on the settings page.
type settingsEntry struct {
	kind        settingsEntryKind
	label       string
	description string
	// value is a short summary of the current value, rendered right-aligned
	// (e.g. the active color-scheme name). Empty for entries without one.
	value string
}

// settingsActivatedMsg is emitted when the user presses Enter on a setting
// entry. The inbox handles it by opening the matching editor.
type settingsActivatedMsg struct {
	kind settingsEntryKind
}

// settingsCloseMsg is emitted when the user dismisses the settings page
// (esc / left-arrow with nowhere to go). The inbox switches back to the
// inbox screen.
type settingsCloseMsg struct{}

// settingsFocusSidebarMsg asks the inbox to move focus to the sidebar
// while keeping the settings page visible (user pressed left arrow).
type settingsFocusSidebarMsg struct{}

// settingsView is the Settings page model. It owns only cursor + list
// state; the inbox owns the actual preference values and re-seeds the
// value column on re-entry.
type settingsView struct {
	entries []settingsEntry
	cursor  int
	width   int
	height  int
	focused bool
}

// newSettingsView builds the settings page with current preference
// values baked in. A non-empty overrideURL renders the active-hub row
// as "overridden (URL)" and disables its toggle.
func newSettingsView(currentColorScheme, currentDefaultBackend, currentActiveHub, remoteHubURL, overrideURL string) settingsView {
	hubLabel := "Active hub"
	hubDesc := "Which clankd this TUI/CLI talks to. Press enter to toggle. Restart the TUI for changes to take effect."
	hubValue := resolveActiveHubName(currentActiveHub, remoteHubURL)
	if strings.TrimSpace(overrideURL) != "" {
		hubDesc = "Pinned by --hub-url for this process; toggle disabled. Restart without the flag to switch via preferences."
		hubValue = "overridden (" + overrideURL + ")"
	}
	return settingsView{
		entries: []settingsEntry{
			{
				kind:        settingsEntryColorScheme,
				label:       "Change color scheme",
				description: "Pick the TUI palette (live preview on hover).",
				value:       resolveColorSchemeName(currentColorScheme),
			},
			{
				kind:        settingsEntryDefaultBackend,
				label:       "Default agent backend",
				description: "Backend used for new sessions when no override is given. Press enter to cycle.",
				value:       resolveDefaultBackendName(currentDefaultBackend),
			},
			{
				kind:        settingsEntryActiveHub,
				label:       hubLabel,
				description: hubDesc,
				value:       hubValue,
			},
		},
	}
}

func (s settingsView) Init() tea.Cmd { return nil }

func (s *settingsView) SetSize(w, h int) {
	s.width = w
	s.height = h
}

func (s *settingsView) SetFocused(f bool) {
	s.focused = f
}

// SetColorSchemeValue updates the "current value" text for the color
// scheme entry. Called after the picker returns so the column stays in
// sync without rebuilding the view.
func (s *settingsView) SetColorSchemeValue(name string) {
	display := resolveColorSchemeName(name)
	for i := range s.entries {
		if s.entries[i].kind == settingsEntryColorScheme {
			s.entries[i].value = display
			return
		}
	}
}

// SetDefaultBackendValue updates the "current value" text for the
// default-backend entry. Called after the user cycles the value so the
// column reflects the new selection without rebuilding the view.
func (s *settingsView) SetDefaultBackendValue(name string) {
	display := resolveDefaultBackendName(name)
	for i := range s.entries {
		if s.entries[i].kind == settingsEntryDefaultBackend {
			s.entries[i].value = display
			return
		}
	}
}

// DefaultBackendValue returns the currently displayed default-backend
// name. Authoritative for cycle decisions: persistence is async, so
// reading from disk would lag behind rapid Enter presses.
func (s *settingsView) DefaultBackendValue() string {
	for _, e := range s.entries {
		if e.kind == settingsEntryDefaultBackend {
			return e.value
		}
	}
	return ""
}

// SetActiveHubValue updates the "current value" text for the active-
// hub entry, mirroring the default-backend pattern.
func (s *settingsView) SetActiveHubValue(activeHub, remoteHubURL string) {
	display := resolveActiveHubName(activeHub, remoteHubURL)
	for i := range s.entries {
		if s.entries[i].kind == settingsEntryActiveHub {
			s.entries[i].value = display
			return
		}
	}
}

// resolveColorSchemeName returns the scheme name to display, falling back
// to the first built-in (the default) when no preference is set.
func resolveColorSchemeName(name string) string {
	if name == "" {
		return builtInSchemes[0].Name
	}
	return name
}

// resolveDefaultBackendName returns the backend name to display.
// Unknown values fall back to agent.DefaultBackend.
func resolveDefaultBackendName(name string) string {
	bt, _ := agent.ResolveBackendPreference(name)
	return string(bt)
}

// resolveActiveHubName renders the active-hub field. "remote" with no
// URL configured renders as "remote (not configured)".
func resolveActiveHubName(activeHub, remoteHubURL string) string {
	switch activeHub {
	case "remote":
		if strings.TrimSpace(remoteHubURL) == "" {
			return "remote (not configured)"
		}
		return "remote (" + remoteHubURL + ")"
	default:
		return "local"
	}
}

func (s settingsView) Update(msg tea.Msg) (settingsView, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return s, nil
	}
	keyMsg = normalizeKeyCase(keyMsg)

	switch {
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("up", "k"))):
		if s.cursor > 0 {
			s.cursor--
		}
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("down", "j"))):
		if s.cursor < len(s.entries)-1 {
			s.cursor++
		}
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("home", "g"))):
		s.cursor = 0
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("end", "G"))):
		s.cursor = len(s.entries) - 1
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("enter"))):
		if s.cursor >= 0 && s.cursor < len(s.entries) {
			kind := s.entries[s.cursor].kind
			return s, func() tea.Msg { return settingsActivatedMsg{kind: kind} }
		}
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("esc"))):
		return s, func() tea.Msg { return settingsCloseMsg{} }
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("left", "h"))):
		return s, func() tea.Msg { return settingsFocusSidebarMsg{} }
	}
	return s, nil
}

// View renders the settings page content. It returns a plain string that
// the inbox composes into the right pane (parallel to renderSessionPane).
func (s settingsView) View() string {
	var sb strings.Builder

	// Header.
	header := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render("Settings")
	sb.WriteString(header)
	sb.WriteString("\n")

	hint := lipgloss.NewStyle().
		Foreground(dimColor).
		Render("↑↓ navigate · enter select · ← sidebar · esc close")
	sb.WriteString(hint)
	sb.WriteString("\n\n")

	innerWidth := s.width - 2 // leave a bit of breathing room
	if innerWidth < 30 {
		innerWidth = 30
	}

	for i, e := range s.entries {
		selected := i == s.cursor && s.focused

		leftLabel := e.label
		rightValue := ""
		if e.value != "" {
			rightValue = lipgloss.NewStyle().Foreground(dimColor).Render(e.value)
		}

		// Compute gap so the value column is right-aligned.
		gap := innerWidth - lipgloss.Width(leftLabel) - lipgloss.Width(rightValue) - 2
		if gap < 1 {
			gap = 1
		}

		var line string
		if selected {
			prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
			styledLabel := lipgloss.NewStyle().Foreground(textColor).Bold(true).Render(leftLabel)
			line = prefix + styledLabel + strings.Repeat(" ", gap) + rightValue
		} else {
			styledLabel := lipgloss.NewStyle().Foreground(textColor).Render(leftLabel)
			line = "  " + styledLabel + strings.Repeat(" ", gap) + rightValue
		}
		sb.WriteString(line)
		sb.WriteString("\n")

		if e.description != "" {
			desc := lipgloss.NewStyle().
				Foreground(mutedColor).
				Italic(true).
				Render("    " + e.description)
			sb.WriteString(desc)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	return sb.String()
}
