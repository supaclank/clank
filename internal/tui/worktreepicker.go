package tui

// Worktree picker modal — a searchable list of the current repo's worktrees,
// so the user can target a different one from the compose view. This is the
// "select an existing worktree" counterpart to the New-worktree toggle (which
// creates a fresh one) and the folder picker (which chooses the repo).
//
// Worktrees come from `git worktree list` on the project directory. The host
// is always local here; a remote-host variant would route through the daemon's
// branch listing instead.

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/supaclank/clank/internal/git"
)

// worktreePickerResultMsg is sent when the user selects a worktree.
type worktreePickerResultMsg struct{ dir string }

// worktreePickerCancelMsg is sent when the user dismisses the picker.
type worktreePickerCancelMsg struct{}

type worktreePickerModel struct {
	all       []git.Worktree // every worktree (unfiltered)
	filtered  []git.Worktree // matching the current query
	current   string         // the worktree dir in use (marked; cursor starts here)
	cursor    int
	scroll    int
	maxRows   int
	search    textinput.Model
	lastQuery string
	err       error
}

func newWorktreePicker(worktrees []git.Worktree, current string, maxRows int, err error) worktreePickerModel {
	if maxRows < 4 {
		maxRows = 4
	}

	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.CharLimit = 128
	ti.Prompt = "/ "
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(dimColor)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)
	ti.SetWidth(52)
	ti.Focus()

	m := worktreePickerModel{all: worktrees, current: current, maxRows: maxRows, search: ti, err: err}
	m.applyFilter()
	for i, w := range m.filtered {
		if w.Path == current {
			m.cursor = i
			break
		}
	}
	m.ensureVisible()
	return m
}

func (m worktreePickerModel) Init() tea.Cmd {
	return func() tea.Msg { return textinput.Blink() }
}

func (m worktreePickerModel) Update(msg tea.Msg) (worktreePickerModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		msg = normalizeKeyCase(msg)
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			return m, func() tea.Msg { return worktreePickerCancelMsg{} }

		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			if m.cursor >= 0 && m.cursor < len(m.filtered) {
				dir := m.filtered[m.cursor].Path
				return m, func() tea.Msg { return worktreePickerResultMsg{dir: dir} }
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

	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			if m.cursor > 0 {
				m.cursor--
				m.ensureVisible()
			}
		case tea.MouseWheelDown:
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
				m.ensureVisible()
			}
		}
		return m, nil

	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		return m, nil
	}

	// Forward to the filter input and re-filter on change.
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	if q := m.search.Value(); q != m.lastQuery {
		m.lastQuery = q
		m.cursor = 0
		m.applyFilter()
	}
	return m, cmd
}

// applyFilter rebuilds the visible list from the query (matched against the
// branch name and the directory path).
func (m *worktreePickerModel) applyFilter() {
	q := strings.ToLower(m.search.Value())
	if q == "" {
		m.filtered = m.all
	} else {
		m.filtered = nil
		for _, w := range m.all {
			if strings.Contains(strings.ToLower(w.Branch), q) || strings.Contains(strings.ToLower(w.Path), q) {
				m.filtered = append(m.filtered, w)
			}
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.scroll = 0
	m.ensureVisible()
}

func (m *worktreePickerModel) ensureVisible() {
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+m.maxRows {
		m.scroll = m.cursor - m.maxRows + 1
	}
}

func (m worktreePickerModel) View() string {
	const menuWidth = 60
	innerWidth := menuWidth - 4

	var sb strings.Builder
	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(textColor).Width(innerWidth).Render("Select worktree"))
	sb.WriteString("\n")
	sb.WriteString(m.search.View())
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(mutedColor).Render(strings.Repeat("─", innerWidth)))
	sb.WriteString("\n")

	switch {
	case m.err != nil:
		// TODO(ai-review): render actual error (trimmed) instead of hard-coded message https://github.com/supaclank/clank/pull/73#discussion_r3449079132
		sb.WriteString(lipgloss.NewStyle().Foreground(dangerColor).Width(innerWidth).Render("  not a git repo"))
		sb.WriteString("\n")
	case len(m.filtered) == 0:
		sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Width(innerWidth).Render("  no matches"))
		sb.WriteString("\n")
	default:
		end := m.scroll + m.maxRows
		if end > len(m.filtered) {
			end = len(m.filtered)
		}
		if m.scroll > 0 {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Width(innerWidth).Render("  ↑ ···"))
			sb.WriteString("\n")
		}
		for i := m.scroll; i < end; i++ {
			selected := i == m.cursor
			label := m.renderItem(m.filtered[i], innerWidth, selected)
			if selected {
				sb.WriteString(lipgloss.NewStyle().Background(primaryColor).Foreground(textColor).Bold(true).Width(innerWidth).Render(label))
			} else {
				sb.WriteString(lipgloss.NewStyle().Width(innerWidth).Render(label))
			}
			sb.WriteString("\n")
		}
		if end < len(m.filtered) {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Width(innerWidth).Render("  ↓ ···"))
			sb.WriteString("\n")
		}
	}

	sb.WriteString(lipgloss.NewStyle().Foreground(dimColor).Render("↑↓ move   type to filter   enter select   esc cancel"))

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())
}

// renderItem shows a worktree's branch, the dot marking the one in use, and a
// dimmed tail of its directory. The selected row is rendered as plain text so
// the cursor's background highlight fills the whole line — inner color spans
// emit resets that would otherwise cut the background off mid-row.
func (m worktreePickerModel) renderItem(w git.Worktree, width int, selected bool) string {
	dot := "  "
	if w.Path == m.current {
		dot = "● "
	}
	name := w.Branch
	if name == "" {
		name = "(detached) " + shortHash(w.Head)
	}

	dirHint := w.Path
	avail := width - 2 - lipgloss.Width(name) - 2
	if avail < 8 {
		avail = 8
	}
	if lipgloss.Width(dirHint) > avail {
		r := []rune(dirHint)
		w := 0
		idx := len(r)
		for idx > 0 {
			rw := lipgloss.Width(string(r[idx-1]))
			if w+rw > avail-1 {
				break
			}
			w += rw
			idx--
		}
		dirHint = "…" + string(r[idx:])
	}

	if selected {
		return dot + name + "  " + dirHint
	}

	dotStr := dot
	if w.Path == m.current {
		dotStr = lipgloss.NewStyle().Foreground(successColor).Render("● ")
	}
	return dotStr +
		lipgloss.NewStyle().Foreground(textColor).Render(name) + "  " +
		lipgloss.NewStyle().Foreground(dimColor).Render(dirHint)
}

func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

// listWorktrees returns the repo's worktrees (excluding the bare entry) for the
// given directory, plus any error from git.
func listWorktrees(dir string) ([]git.Worktree, error) {
	all, err := git.ListWorktrees(dir)
	if err != nil {
		return nil, err
	}
	out := make([]git.Worktree, 0, len(all))
	for _, w := range all {
		if w.Bare {
			continue
		}
		out = append(out, w)
	}
	return out, nil
}
