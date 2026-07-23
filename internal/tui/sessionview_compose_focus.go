package tui

// Keyboard focus navigation for the compose view. The user moves a
// vibrant marker between the Backend / Project rows and the prompt box
// with the arrow keys; each row is operated in place (switch backend,
// open a picker) without leaving the keyboard.

import (
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/agent"
)

// composeFocus identifies a focusable row in the compose view.
type composeFocus int

const (
	// focusPrompt is the zero value so a freshly-constructed compose
	// view starts with the textarea focused, as it did before navigation.
	focusPrompt composeFocus = iota
	focusBackend
	focusFolder
	focusWorktree
	focusNewWorktree
)

// composeRows returns the focusable rows top-to-bottom. The prompt box
// is always last; rows above it become interactive as later phases land.
func (m *SessionViewModel) composeRows() []composeFocus {
	return []composeFocus{focusBackend, focusFolder, focusWorktree, focusNewWorktree, focusPrompt}
}

// indexOfComposeFocus returns the position of f within rows, defaulting
// to the prompt (last) when f isn't currently navigable.
func indexOfComposeFocus(rows []composeFocus, f composeFocus) int {
	for i, r := range rows {
		if r == f {
			return i
		}
	}
	return len(rows) - 1
}

// moveComposeFocus shifts focus by delta (-1 up, +1 down) over the
// visible rows, clamping at both ends.
func (m *SessionViewModel) moveComposeFocus(delta int) tea.Cmd {
	rows := m.composeRows()
	next := indexOfComposeFocus(rows, m.composeFocus) + delta
	if next < 0 || next >= len(rows) {
		return nil
	}
	return m.setComposeFocus(rows[next])
}

// setComposeFocus moves focus to f, focusing the textarea only when the
// prompt row is the target and blurring it otherwise so the chevron cursor
// highlights the active row instead.
func (m *SessionViewModel) setComposeFocus(f composeFocus) tea.Cmd {
	m.composeFocus = f
	if f == focusBackend {
		// Start the horizontal cursor on the currently-active backend.
		m.backendCursor = backendCursorIndex(m.backend)
	}
	if f == focusPrompt {
		return m.input.Focus()
	}
	m.input.Blur()
	return nil
}

// backendCursorIndex maps a backend to its horizontal cursor position
// in the AllBackends display order.
func backendCursorIndex(b agent.BackendType) int {
	if i := slices.Index(agent.AllBackends, b); i >= 0 {
		return i
	}
	return 0
}

// backendForCursor is the inverse of backendCursorIndex.
func backendForCursor(i int) agent.BackendType {
	if i >= 0 && i < len(agent.AllBackends) {
		return agent.AllBackends[i]
	}
	return agent.DefaultBackend
}

// handleComposeNavKey handles keys while a non-prompt row is focused.
func (m *SessionViewModel) handleComposeNavKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// On a label row "q" quits, matching the inbox. Checked first so it isn't
	// swallowed by the folder row's type-to-open-picker behavior.
	if key.Matches(msg, key.NewBinding(key.WithKeys("q", "Q"))) {
		return m, tea.Quit
	}

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
		return m, m.moveComposeFocus(-1)
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
		return m, m.moveComposeFocus(1)
	}

	switch m.composeFocus {
	case focusBackend:
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("left", "h"))):
			m.backendCursor = 0
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("right", "l"))):
			m.backendCursor = 1
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			// Commit the hovered backend and stay on the row so the green
			// active marker visibly shifts.
			return m, m.applyBackend(backendForCursor(m.backendCursor))
		}
	case focusFolder:
		// Only Enter opens the folder picker — other keys pass through so
		// global shortcuts keep working on this row.
		if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
			return m, m.openFolderPicker()
		}
	case focusWorktree:
		// Enter opens the worktree picker (existing worktrees of this repo).
		if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
			return m, m.openWorktreePicker()
		}
	case focusNewWorktree:
		// Enter / Space flips the toggle in place.
		if key.Matches(msg, key.NewBinding(key.WithKeys("enter", " ", "space"))) {
			m.isNewWorktree = !m.isNewWorktree
			return m, nil
		}
	}
	return m, nil
}

// applyBackend switches to backend b and rebuilds the per-backend model
// and mode state, returning the catalog-refetch command. No-op (nil cmd)
// when b is already selected so re-selecting doesn't flicker the lists.
//
// Each backend has its own model catalog (claude-code is a closed
// sonnet/opus/haiku enum; opencode aggregates the user's configured
// providers). Dropping the stale list and refetching prevents submitting
// the previous backend's selectedModel index, which would be forwarded as
// `--model <opencode-id>` to the claude CLI and crash on spawn.
func (m *SessionViewModel) applyBackend(b agent.BackendType) tea.Cmd {
	if m.backend == b {
		return nil
	}
	m.backend = b
	m.models = nil
	m.selectedModel = -1
	switch b {
	case agent.BackendClaudeCode:
		m.modes, m.selectedMode = claudePermissionModes()
		return m.fetchModels()
	case agent.BackendOpenCode:
		m.modes, m.selectedMode = nil, 0
		return tea.Batch(m.fetchAgents(), m.fetchModels())
	default:
		// ACP-served backends own their mode vocabulary — modes appear
		// in-session once the agent advertises them (session/new).
		m.modes, m.selectedMode = nil, 0
		return m.fetchModels()
	}
}

// toggleBackend cycles through AllBackends (the ctrl+b shortcut).
func (m *SessionViewModel) toggleBackend() tea.Cmd {
	next := backendForCursor((backendCursorIndex(m.backend) + 1) % len(agent.AllBackends))
	return m.applyBackend(next)
}

// composeFieldSpec is one row in the compose field grid: a left-hand label
// and the value being edited, tied to the focus state that highlights it.
type composeFieldSpec struct {
	label string
	value string
	focus composeFocus
}

// composeFocusChevron is the 2-cell cursor prefix: a vibrant chevron when
// the row holds focus, two spaces otherwise. Equal width either way so the
// layout never shifts as focus moves.
func composeFocusChevron(focused bool) string {
	if focused {
		return lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("› ")
	}
	return "  "
}

// renderComposeFields renders the compose fields as plain "label: value"
// lines. Focus is shown by the vibrant chevron cursor and a recolored
// label — no borders, so there's nothing to leak between rows and the
// layout never shifts.
func (m *SessionViewModel) renderComposeFields(fields []composeFieldSpec) string {
	const labelW = 14
	var b strings.Builder
	for i, f := range fields {
		focused := m.composeFocus == f.focus
		labelSty := lipgloss.NewStyle().Foreground(dimColor).Width(labelW)
		if focused {
			labelSty = lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Width(labelW)
		}
		b.WriteString(composeFocusChevron(focused) + labelSty.Render(f.label+":") + " " + f.value)
		if i < len(fields)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}
