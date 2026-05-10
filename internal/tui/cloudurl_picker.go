package tui

// cloudurl_picker.go — modal for selecting or entering the cloud base URL.
// Presents a list of known providers and a "Custom URL" option; selecting
// "Custom URL" transitions to a text-input phase.
//
// Phase machine:
//
//	cloudURLPhaseList  → cloudURLPhaseInput (Custom selected)
//	cloudURLPhaseList  → done (known provider selected, Enter)
//	cloudURLPhaseInput → done (Enter with non-empty value)
//	any phase          → cancel (Esc)

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// knownCloudProviders is the ordered list of pre-configured cloud URLs
// shown in the picker list. Append entries here as new hosted deployments
// become available.
var knownCloudProviders = []cloudProvider{}

type cloudProvider struct {
	Name string
	URL  string
}

// customCloudProviderLabel is the sentinel row that opens the text-input.
const customCloudProviderLabel = "Custom URL…"

// cloudURLPhase is the picker's internal state machine.
type cloudURLPhase int

const (
	cloudURLPhaseList  cloudURLPhase = iota // browsing the provider list
	cloudURLPhaseInput                      // typing a custom URL
)

// cloudURLPickerResultMsg is emitted when the user confirms a selection.
type cloudURLPickerResultMsg struct{ url string }

// cloudURLPickerCancelMsg is emitted when the user presses Esc.
type cloudURLPickerCancelMsg struct{}

// cloudURLPickerModel is the modal model.
type cloudURLPickerModel struct {
	phase   cloudURLPhase
	cursor  int
	input   textinput.Model
	current string // the URL that was active when the picker opened
}

func newCloudURLPicker(currentURL string) cloudURLPickerModel {
	ti := textinput.New()
	ti.Placeholder = "https://your-cloud.example.com"
	ti.CharLimit = 512
	ti.Prompt = "› "
	ti.SetWidth(52)
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(primaryColor)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)
	// Pre-fill with the current value so the user can edit rather than retype.
	if currentURL != "" {
		ti.SetValue(currentURL)
	}

	return cloudURLPickerModel{
		phase:   cloudURLPhaseList,
		input:   ti,
		current: currentURL,
	}
}

func (m cloudURLPickerModel) Init() tea.Cmd { return nil }

func (m cloudURLPickerModel) Update(msg tea.Msg) (cloudURLPickerModel, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		if m.phase == cloudURLPhaseInput {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		return m, nil
	}
	keyMsg = normalizeKeyCase(keyMsg)

	switch m.phase {
	case cloudURLPhaseList:
		return m.handleListKey(keyMsg)
	case cloudURLPhaseInput:
		return m.handleInputKey(keyMsg)
	}
	return m, nil
}

func (m cloudURLPickerModel) handleListKey(msg tea.KeyPressMsg) (cloudURLPickerModel, tea.Cmd) {
	rowCount := len(knownCloudProviders) + 1 // +1 for the Custom row
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
		if m.cursor > 0 {
			m.cursor--
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
		if m.cursor < rowCount-1 {
			m.cursor++
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		if m.cursor < len(knownCloudProviders) {
			// Known provider selected — emit result immediately.
			url := knownCloudProviders[m.cursor].URL
			return m, func() tea.Msg { return cloudURLPickerResultMsg{url: url} }
		}
		// Custom URL — switch to text-input phase.
		m.phase = cloudURLPhaseInput
		return m, m.input.Focus()
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		return m, func() tea.Msg { return cloudURLPickerCancelMsg{} }
	}
	return m, nil
}

func (m cloudURLPickerModel) handleInputKey(msg tea.KeyPressMsg) (cloudURLPickerModel, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		url := strings.TrimSpace(m.input.Value())
		return m, func() tea.Msg { return cloudURLPickerResultMsg{url: url} }
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		// Return to list rather than closing the whole picker.
		m.phase = cloudURLPhaseList
		m.input.Blur()
		return m, nil
	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
}

func (m cloudURLPickerModel) View() string {
	var sb strings.Builder

	title := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("Set Cloud URL")
	sb.WriteString(title)
	sb.WriteString("\n\n")

	switch m.phase {
	case cloudURLPhaseList:
		sb.WriteString(m.viewList())
	case cloudURLPhaseInput:
		sb.WriteString(m.viewInput())
	}

	return sb.String()
}

func (m cloudURLPickerModel) viewList() string {
	var sb strings.Builder

	hint := lipgloss.NewStyle().Foreground(dimColor).Render("↑↓ navigate · enter select · esc cancel")
	sb.WriteString(hint)
	sb.WriteString("\n\n")

	for i, p := range knownCloudProviders {
		sb.WriteString(m.renderRow(i, p.Name, p.URL))
	}
	// Custom URL row is always last.
	customIdx := len(knownCloudProviders)
	sb.WriteString(m.renderRow(customIdx, customCloudProviderLabel, ""))

	return sb.String()
}

func (m cloudURLPickerModel) renderRow(idx int, label, value string) string {
	selected := idx == m.cursor
	var line string
	if selected {
		prefix := lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
		styledLabel := lipgloss.NewStyle().Foreground(textColor).Bold(true).Render(label)
		styledValue := ""
		if value != "" {
			styledValue = "  " + lipgloss.NewStyle().Foreground(dimColor).Render(value)
		}
		line = prefix + styledLabel + styledValue
	} else {
		styledLabel := lipgloss.NewStyle().Foreground(textColor).Render(label)
		styledValue := ""
		if value != "" {
			styledValue = "  " + lipgloss.NewStyle().Foreground(dimColor).Render(value)
		}
		line = "  " + styledLabel + styledValue
	}
	return line + "\n"
}

func (m cloudURLPickerModel) viewInput() string {
	var sb strings.Builder

	prompt := lipgloss.NewStyle().Foreground(textColor).Render("Enter a custom cloud URL:")
	sb.WriteString(prompt)
	sb.WriteString("\n\n")
	sb.WriteString(m.input.View())
	sb.WriteString("\n")
	sb.WriteString("\n")
	hint := lipgloss.NewStyle().Foreground(dimColor).Render("enter confirm · esc back to list · leave empty to disable cloud")
	sb.WriteString(hint)

	return sb.String()
}
