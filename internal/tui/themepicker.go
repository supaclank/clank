package tui

// themepicker.go — live-preview color scheme picker overlay.
//
// Modeled on modelpicker.go but without the search field (the list is
// short). Arrow navigation immediately calls applyColorScheme so the
// background view reflects the hovered palette. Enter persists; Esc
// restores the scheme that was active when the picker was opened.

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// themePickerResultMsg is sent when the user confirms a scheme with Enter.
type themePickerResultMsg struct {
	scheme string
}

// themePickerCancelMsg is sent when the user dismisses the picker with Esc.
type themePickerCancelMsg struct{}

type themePickerModel struct {
	items   []ColorScheme
	cursor  int
	scroll  int
	maxRows int
	// original is the scheme that was active when the picker opened —
	// Esc re-applies it so the "preview on hover" semantics don't leak
	// out of the picker.
	original ColorScheme
	// currentName is the scheme name saved in preferences (used for the
	// active indicator ● in the rendered list).
	currentName string
}

func newThemePicker(currentName string) themePickerModel {
	// Snapshot the active scheme so Esc can revert.
	orig, _ := schemeByName(currentName)

	p := themePickerModel{
		items:       builtInSchemes,
		maxRows:     12,
		original:    orig,
		currentName: currentName,
	}
	if currentName == "" {
		p.currentName = builtInSchemes[0].Name
	}
	for i, s := range p.items {
		if s.Name == p.currentName {
			p.cursor = i
			break
		}
	}
	p.ensureVisible()
	// Apply the current scheme once so a mis-aligned palette (e.g. after
	// a restart mid-preview in theory) is corrected on open.
	applyColorScheme(p.items[p.cursor])
	return p
}

func (m themePickerModel) Init() tea.Cmd { return nil }

func (m themePickerModel) Update(msg tea.Msg) (themePickerModel, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	keyMsg = normalizeKeyCase(keyMsg)

	switch {
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("esc"))):
		// Revert to the scheme that was active on open.
		applyColorScheme(m.original)
		return m, func() tea.Msg { return themePickerCancelMsg{} }

	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("enter"))):
		if m.cursor >= 0 && m.cursor < len(m.items) {
			name := m.items[m.cursor].Name
			return m, func() tea.Msg { return themePickerResultMsg{scheme: name} }
		}
		return m, nil

	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("up", "k", "ctrl+p"))):
		if m.cursor > 0 {
			m.cursor--
			m.ensureVisible()
			applyColorScheme(m.items[m.cursor])
		}
		return m, nil

	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("down", "j", "ctrl+n"))):
		if m.cursor < len(m.items)-1 {
			m.cursor++
			m.ensureVisible()
			applyColorScheme(m.items[m.cursor])
		}
		return m, nil
	}
	return m, nil
}

func (m *themePickerModel) ensureVisible() {
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+m.maxRows {
		m.scroll = m.cursor - m.maxRows + 1
	}
}

// View renders the picker as a bordered overlay with a small palette
// swatch beside each entry so the user can see what each scheme looks
// like without having to hover every option.
func (m themePickerModel) View() string {
	var sb strings.Builder

	menuWidth := 48
	innerWidth := menuWidth - 4

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(textColor).
		Width(innerWidth).
		Render("Color Scheme")
	sb.WriteString(title)
	sb.WriteString("\n")

	sb.WriteString(lipgloss.NewStyle().
		Foreground(mutedColor).
		Render(strings.Repeat("─", innerWidth)))
	sb.WriteString("\n")

	end := m.scroll + m.maxRows
	if end > len(m.items) {
		end = len(m.items)
	}
	for i := m.scroll; i < end; i++ {
		s := m.items[i]

		// Cursor marker: always visible, independent of background color.
		// Live-preview swaps primaryColor to match the hovered scheme, so a
		// background-only highlight can vanish when primary ≈ text. The
		// leading "> " in secondaryColor keeps the cursor position obvious
		// in every scheme.
		cursorMark := "  "
		if i == m.cursor {
			cursorMark = lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render("> ")
		}

		// Persisted-scheme marker (the one saved in preferences). Shown in
		// successColor so it stands out even when primary shifts during
		// preview.
		indicator := "   "
		if s.Name == m.currentName {
			indicator = " " + lipgloss.NewStyle().Foreground(successColor).Bold(true).Render("* ")
		}

		swatch := renderSwatch(s)
		name := s.Name

		swatchW := lipgloss.Width(swatch)
		gap := innerWidth - 2 /*cursorMark*/ - swatchW - lipgloss.Width(name) - 3 /*indicator*/ - 1
		if gap < 1 {
			gap = 1
		}
		row := cursorMark + swatch + " " + name + strings.Repeat(" ", gap) + indicator

		if i == m.cursor {
			line := lipgloss.NewStyle().
				Foreground(textColor).
				Bold(true).
				Render(row)
			sb.WriteString(line)
		} else {
			line := lipgloss.NewStyle().
				Foreground(textColor).
				Render(row)
			sb.WriteString(line)
		}
		sb.WriteString("\n")
	}

	hint := lipgloss.NewStyle().
		Foreground(dimColor).
		Render("↑↓ preview  enter save  esc cancel")
	sb.WriteString(hint)

	popup := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())

	return popup
}

// renderSwatch returns a short colored swatch that represents the
// scheme's palette — useful for skimming the list without navigating.
func renderSwatch(s ColorScheme) string {
	block := "██"
	b := func(hex string) string {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render(block)
	}
	return b(s.Primary) + b(s.Secondary) + b(s.Success) + b(s.Warning) + b(s.Danger)
}
