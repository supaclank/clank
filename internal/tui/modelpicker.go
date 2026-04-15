package tui

// Model picker modal — a searchable overlay for selecting an LLM model.

import (
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// modelPickerResultMsg is sent when the user selects a model.
// selectedModel is -1 for "(default)", otherwise the index into the
// original (unfiltered) models slice.
type modelPickerResultMsg struct {
	selectedModel int
}

// modelPickerCancelMsg is sent when the user dismisses the picker.
type modelPickerCancelMsg struct{}

// modelPickerItem is a single entry in the picker list. Index -1 is
// the synthetic "(default)" entry.
type modelPickerItem struct {
	index      int    // index into the original models slice; -1 = default
	modelID    string // e.g. "claude-opus-4-20250514"
	providerID string // e.g. "github-copilot"
	display    string // pre-built display string for matching: "model  provider"
}

type modelPickerModel struct {
	items     []modelPickerItem // all items (unfiltered)
	filtered  []modelPickerItem // items matching the current query
	cursor    int               // index into filtered
	scroll    int               // first visible row in the list
	search    textinput.Model   // search input field
	lastQuery string            // tracks previous value to detect changes
	maxRows   int               // max visible rows before scrolling
	selected  int               // currently active model index (for highlight)
	backend   agent.BackendType // backend type (controls provider config hint)
}

func newModelPicker(models []agent.ModelInfo, selected int, backend agent.BackendType) modelPickerModel {
	// Sort models by provider then name for a stable order.
	sorted := make([]agent.ModelInfo, len(models))
	copy(sorted, models)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ProviderID != sorted[j].ProviderID {
			return sorted[i].ProviderID < sorted[j].ProviderID
		}
		return sorted[i].ID < sorted[j].ID
	})

	// Build a mapping from sorted back to original indices so we can return
	// the correct index in the result message.
	origIndex := make(map[string]int, len(models))
	for i, m := range models {
		origIndex[m.ProviderID+"\x00"+m.ID] = i
	}

	items := make([]modelPickerItem, 0, len(sorted)+1)
	// Synthetic "(default)" entry.
	items = append(items, modelPickerItem{
		index:   -1,
		display: "(default)",
	})
	for _, m := range sorted {
		idx := origIndex[m.ProviderID+"\x00"+m.ID]
		items = append(items, modelPickerItem{
			index:      idx,
			modelID:    m.ID,
			providerID: m.ProviderID,
			display:    m.ID + "  " + m.ProviderID,
		})
	}

	// Set up the search text input.
	ti := textinput.New()
	ti.Placeholder = "filter..."
	ti.CharLimit = 128
	ti.Prompt = "/ "
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(dimColor)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)
	ti.SetWidth(48) // menuWidth(52) - border+padding(4)
	ti.Focus()

	picker := modelPickerModel{
		items:    items,
		maxRows:  15,
		selected: selected,
		search:   ti,
		backend:  backend,
	}
	picker.applyFilter()

	// Place cursor on the currently selected model.
	for i, item := range picker.filtered {
		if item.index == selected {
			picker.cursor = i
			break
		}
	}
	picker.ensureVisible()

	return picker
}

func (m modelPickerModel) Init() tea.Cmd {
	return nil
}

func (m modelPickerModel) Update(msg tea.Msg) (modelPickerModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		msg = normalizeKeyCase(msg)
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			return m, func() tea.Msg { return modelPickerCancelMsg{} }

		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			if m.cursor >= 0 && m.cursor < len(m.filtered) {
				idx := m.filtered[m.cursor].index
				return m, func() tea.Msg { return modelPickerResultMsg{selectedModel: idx} }
			}
			return m, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
			if m.cursor > 0 {
				m.cursor--
				m.ensureVisible()
			}
			return m, nil

		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
				m.ensureVisible()
			}
			return m, nil
		}
	}

	// Forward all other messages to the text input.
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)

	// Re-filter when the query changes.
	if q := m.search.Value(); q != m.lastQuery {
		m.lastQuery = q
		m.applyFilter()
	}

	return m, cmd
}

// applyFilter rebuilds the filtered list from the query and resets cursor.
func (m *modelPickerModel) applyFilter() {
	q := strings.ToLower(m.search.Value())
	if q == "" {
		m.filtered = m.items
	} else {
		m.filtered = nil
		for _, item := range m.items {
			if strings.Contains(strings.ToLower(item.display), q) {
				m.filtered = append(m.filtered, item)
			}
		}
	}
	// Keep cursor in bounds.
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.scroll = 0
	m.ensureVisible()
}

// ensureVisible adjusts scroll so the cursor is within the visible window.
func (m *modelPickerModel) ensureVisible() {
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+m.maxRows {
		m.scroll = m.cursor - m.maxRows + 1
	}
}

func (m modelPickerModel) View() string {
	var sb strings.Builder

	menuWidth := 52
	innerWidth := menuWidth - 4 // border + padding

	// Title.
	titleStr := lipgloss.NewStyle().
		Bold(true).
		Foreground(textColor).
		Width(innerWidth).
		Render("Select Model")
	sb.WriteString(titleStr)
	sb.WriteString("\n")

	// Search field (rendered by textinput).
	sb.WriteString(m.search.View())
	sb.WriteString("\n")

	// Separator.
	sb.WriteString(lipgloss.NewStyle().
		Foreground(mutedColor).
		Render(strings.Repeat("─", innerWidth)))
	sb.WriteString("\n")

	if len(m.filtered) == 0 {
		sb.WriteString(lipgloss.NewStyle().
			Foreground(dimColor).
			Width(innerWidth).
			Render("  no matches"))
		sb.WriteString("\n")
	} else {
		end := m.scroll + m.maxRows
		if end > len(m.filtered) {
			end = len(m.filtered)
		}

		// Scroll indicator (top).
		if m.scroll > 0 {
			sb.WriteString(lipgloss.NewStyle().
				Foreground(dimColor).
				Width(innerWidth).
				Render("  ↑ " + strings.Repeat("·", 3)))
			sb.WriteString("\n")
		}

		for i := m.scroll; i < end; i++ {
			item := m.filtered[i]
			label := m.renderItem(item, innerWidth)

			if i == m.cursor {
				line := lipgloss.NewStyle().
					Background(primaryColor).
					Foreground(textColor).
					Bold(true).
					Width(innerWidth).
					Render(label)
				sb.WriteString(line)
			} else {
				line := lipgloss.NewStyle().
					Foreground(textColor).
					Width(innerWidth).
					Render(label)
				sb.WriteString(line)
			}
			sb.WriteString("\n")
		}

		// Scroll indicator (bottom).
		if end < len(m.filtered) {
			sb.WriteString(lipgloss.NewStyle().
				Foreground(dimColor).
				Width(innerWidth).
				Render("  ↓ " + strings.Repeat("·", 3)))
			sb.WriteString("\n")
		}
	}

	// Hint line.
	hint := lipgloss.NewStyle().
		Foreground(dimColor).
		Render("↑↓ navigate  type to filter  enter select  esc cancel")
	sb.WriteString(hint)

	// Informational note (OpenCode only).
	if m.backend == agent.BackendOpenCode {
		sb.WriteString("\n")
		note := lipgloss.NewStyle().
			Foreground(mutedColor).
			Italic(true).
			Render("missing a model? connect providers via ctrl+p in opencode")
		sb.WriteString("\n")
		sb.WriteString(note)
	}

	popup := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())

	return popup
}

// renderItem builds the display string for a single item row.
func (m modelPickerModel) renderItem(item modelPickerItem, width int) string {
	if item.index == -1 {
		// Default entry.
		label := "(default)"
		if m.selected == -1 {
			label += "  ●"
		}
		return label
	}

	providerSuffix := lipgloss.NewStyle().Foreground(dimColor).Render(item.providerID)
	suffixWidth := lipgloss.Width(providerSuffix)
	modelName := item.modelID

	// Active indicator.
	indicator := ""
	if item.index == m.selected {
		indicator = "  ●"
	}

	nameWidth := width - suffixWidth - len(indicator) - 2 // 2 = gap
	if nameWidth < 4 {
		nameWidth = 4
	}
	if len(modelName) > nameWidth {
		modelName = modelName[:nameWidth-3] + "..."
	}

	gap := width - lipgloss.Width(modelName) - suffixWidth - len(indicator)
	if gap < 1 {
		gap = 1
	}

	return modelName + strings.Repeat(" ", gap) + providerSuffix + indicator
}
