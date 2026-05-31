package clankcli

import (
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/clanksync/triggers"
)

// errHarnessSelectionCanceled is returned by pickHarnesses when the user
// aborts the multiselect (esc / ctrl+c / q).
var errHarnessSelectionCanceled = errors.New("harness selection canceled")

type harnessOption struct {
	label    string
	value    string
	selected bool
}

// harnessSelectModel is a small bubbletea multiselect: ↑/↓ to move, space
// to toggle, enter to confirm (requires ≥1 selected), esc/ctrl+c/q to
// cancel. Both options start selected so a bare Enter accepts "both".
type harnessSelectModel struct {
	options  []harnessOption
	cursor   int
	done     bool
	canceled bool
}

func newHarnessSelectModel() harnessSelectModel {
	return harnessSelectModel{
		options: []harnessOption{
			{label: "Claude Code (CLI / Agent SDK)", value: triggers.HarnessClaudeCode, selected: true},
			{label: "opencode", value: triggers.HarnessOpenCode, selected: true},
		},
	}
}

func (m harnessSelectModel) Init() tea.Cmd { return nil }

func (m harnessSelectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch {
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("up", "k"))):
		if m.cursor > 0 {
			m.cursor--
		} else {
			m.cursor = len(m.options) - 1
		}
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("down", "j"))):
		if m.cursor < len(m.options)-1 {
			m.cursor++
		} else {
			m.cursor = 0
		}
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("space", " "))):
		m.options[m.cursor].selected = !m.options[m.cursor].selected
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("enter"))):
		// Refuse an empty confirm — autopush needs at least one harness.
		if m.anySelected() {
			m.done = true
			return m, tea.Quit
		}
	case key.Matches(keyMsg, key.NewBinding(key.WithKeys("esc", "ctrl+c", "q"))):
		m.canceled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m harnessSelectModel) View() tea.View {
	// Disappear once we're exiting so the picker doesn't linger on screen.
	if m.done || m.canceled {
		return tea.NewView("")
	}
	var b strings.Builder
	b.WriteString("Select coding agents to auto-sync sessions for:\n\n")
	for i, o := range m.options {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		check := "[ ]"
		if o.selected {
			check = "[x]"
		}
		fmt.Fprintf(&b, "%s%s %s\n", cursor, check, o.label)
	}
	b.WriteString("\n↑/↓ move · space toggle · enter confirm · esc cancel\n")
	return tea.NewView(b.String())
}

func (m harnessSelectModel) anySelected() bool {
	for _, o := range m.options {
		if o.selected {
			return true
		}
	}
	return false
}

func (m harnessSelectModel) selectedValues() []string {
	out := make([]string, 0, len(m.options))
	for _, o := range m.options {
		if o.selected {
			out = append(out, o.value)
		}
	}
	return out
}

// pickHarnesses runs the interactive multiselect on the controlling
// terminal and returns the chosen harnesses, or errHarnessSelectionCanceled
// if the user aborted. Only call when isInteractive(cmd) is true.
func pickHarnesses(cmd *cobra.Command) ([]string, error) {
	final, err := tea.NewProgram(
		newHarnessSelectModel(),
		tea.WithInput(cmd.InOrStdin()),
		tea.WithOutput(cmd.OutOrStdout()),
	).Run()
	if err != nil {
		return nil, fmt.Errorf("harness selection: %w", err)
	}
	res, ok := final.(harnessSelectModel)
	if !ok {
		return nil, fmt.Errorf("harness selection: unexpected model %T", final)
	}
	if res.canceled {
		return nil, errHarnessSelectionCanceled
	}
	return res.selectedValues(), nil
}
