package tui

// BranchPaneModel is the left pane of the two-pane inbox layout.
// It shows local git branches for the current project, with visual
// indicators for worktrees, the default branch, and the current branch.
// Users can navigate with up/down and press 'n' to create a new branch.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/daemon"
)

// branchPaneWidth is the fixed width of the branch pane (including border).
const branchPaneWidth = 30

// branchLoadedMsg carries the result of loading branches from the daemon.
type branchLoadedMsg struct {
	branches []daemon.BranchInfo
	err      error
}

// branchWorktreeCreatedMsg is sent after a worktree is created for a new branch.
type branchWorktreeCreatedMsg struct {
	branch string
	err    error
}

// BranchPaneModel displays local git branches and allows selection.
type BranchPaneModel struct {
	client     *daemon.Client
	projectDir string

	branches []daemon.BranchInfo
	cursor   int
	scroll   int

	// Session counts per branch (set by the inbox when sessions are loaded).
	sessionCounts map[string]int // branch name -> active session count

	// New branch input mode.
	creating bool
	input    textinput.Model

	// "All branches" is the virtual first entry (index -1 means all).
	// cursor==0 means "All branches", cursor>=1 means branches[cursor-1].
	focused bool
	width   int
	height  int
	err     error
}

// NewBranchPaneModel creates a branch pane for the given project directory.
func NewBranchPaneModel(client *daemon.Client, projectDir string) BranchPaneModel {
	ti := textinput.New()
	ti.Placeholder = "branch-name"
	ti.CharLimit = 128
	ti.Prompt = "+ "
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(successColor).Bold(true)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)

	return BranchPaneModel{
		client:     client,
		projectDir: projectDir,
		input:      ti,
		cursor:     0, // "All branches" selected by default
	}
}

// Init fetches branches from the daemon.
func (m *BranchPaneModel) Init() tea.Cmd {
	return m.loadBranches()
}

// SelectedBranch returns the currently selected branch name.
// Empty string means "all branches" (no filter).
func (m *BranchPaneModel) SelectedBranch() string {
	if m.cursor == 0 || len(m.branches) == 0 {
		return ""
	}
	idx := m.cursor - 1
	if idx >= len(m.branches) {
		return ""
	}
	return m.branches[idx].Name
}

// SetFocused sets whether this pane has keyboard focus.
func (m *BranchPaneModel) SetFocused(focused bool) {
	m.focused = focused
}

// Focused returns whether the pane has keyboard focus.
func (m *BranchPaneModel) Focused() bool {
	return m.focused
}

// SetSize sets the pane dimensions.
func (m *BranchPaneModel) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// SetSessionCounts updates the per-branch session counts displayed in the pane.
func (m *BranchPaneModel) SetSessionCounts(counts map[string]int) {
	m.sessionCounts = counts
}

// Update handles messages for the branch pane.
func (m *BranchPaneModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case branchLoadedMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.branches = msg.branches
			m.err = nil
		}
		return nil

	case branchWorktreeCreatedMsg:
		if msg.err != nil {
			m.err = msg.err
			return nil
		}
		m.creating = false
		m.input.SetValue("")
		// Reload branches to show the new worktree.
		return m.loadBranches()
	}

	if m.creating {
		return m.updateCreating(msg)
	}

	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		return m.handleKey(keyMsg)
	}

	return nil
}

// handleKey handles keyboard input when focused and not creating.
func (m *BranchPaneModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	msg = normalizeKeyCase(msg)

	maxIdx := len(m.branches) // 0 = "All", 1..len = branches

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
		if m.cursor > 0 {
			m.cursor--
			m.ensureVisible()
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
		if m.cursor < maxIdx {
			m.cursor++
			m.ensureVisible()
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("home", "g"))):
		m.cursor = 0
		m.ensureVisible()
	case key.Matches(msg, key.NewBinding(key.WithKeys("end", "G"))):
		m.cursor = maxIdx
		m.ensureVisible()
	case key.Matches(msg, key.NewBinding(key.WithKeys("n"))):
		m.creating = true
		m.input.SetValue("")
		return m.input.Focus()
	case key.Matches(msg, key.NewBinding(key.WithKeys("r"))):
		return m.loadBranches()
	}

	return nil
}

// updateCreating handles input while creating a new branch.
func (m *BranchPaneModel) updateCreating(msg tea.Msg) tea.Cmd {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		keyMsg = normalizeKeyCase(keyMsg)

		switch {
		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("esc"))):
			m.creating = false
			m.input.SetValue("")
			return nil

		case key.Matches(keyMsg, key.NewBinding(key.WithKeys("enter"))):
			name := strings.TrimSpace(m.input.Value())
			if name == "" {
				m.creating = false
				return nil
			}
			return m.createWorktree(name)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// View renders the branch pane.
func (m *BranchPaneModel) View() string {
	w := m.width
	if w <= 0 {
		w = branchPaneWidth
	}

	// Content width is pane width minus border (2) minus padding (2).
	contentWidth := w - 4
	if contentWidth < 10 {
		contentWidth = 10
	}

	var lines []string

	// Header.
	header := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Render("Worktrees")
	lines = append(lines, header)
	lines = append(lines, "")

	// "All" entry (no filter).
	allLabel := "  All"
	if m.cursor == 0 && m.focused {
		allLabel = lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ") +
			lipgloss.NewStyle().Foreground(textColor).Bold(true).Render("All")
	} else if m.cursor == 0 {
		allLabel = lipgloss.NewStyle().Foreground(textColor).Render("  All")
	} else {
		allLabel = lipgloss.NewStyle().Foreground(dimColor).Render("  All")
	}
	lines = append(lines, allLabel)

	// Branch entries.
	for i, b := range m.branches {
		idx := i + 1 // cursor index (0 = All)
		lines = append(lines, m.renderBranch(b, idx, contentWidth))
	}

	// New branch input.
	if m.creating {
		lines = append(lines, "")
		m.input.SetWidth(contentWidth - 2)
		lines = append(lines, "  "+m.input.View())
	}

	// Error.
	if m.err != nil {
		lines = append(lines, "")
		errLine := lipgloss.NewStyle().Foreground(dangerColor).
			Render(truncateStr(m.err.Error(), contentWidth))
		lines = append(lines, errLine)
	}

	content := strings.Join(lines, "\n")

	// Wrap in a border. Focused: visible rounded border. Unfocused: hidden
	// border (same spacing, no visible line) to avoid visual clutter.
	border := lipgloss.HiddenBorder()
	borderColor := mutedColor
	if m.focused {
		border = lipgloss.RoundedBorder()
		borderColor = primaryColor
	}
	style := lipgloss.NewStyle().
		Border(border).
		BorderForeground(borderColor).
		Width(contentWidth).
		Height(m.listHeight())

	return style.Render(content)
}

// renderBranch renders a single worktree entry with diff stats.
func (m *BranchPaneModel) renderBranch(b daemon.BranchInfo, idx, maxWidth int) string {
	selected := m.cursor == idx && m.focused

	// Diff stat string: "+123 -45" or empty for the default branch.
	var diffStr string
	if b.LinesAdded > 0 || b.LinesRemoved > 0 {
		diffStr = fmt.Sprintf("+%d -%d", b.LinesAdded, b.LinesRemoved)
	}

	// Session count badge.
	count := m.sessionCounts[b.Name]
	countBadge := ""
	if count > 0 {
		countBadge = fmt.Sprintf(" (%d)", count)
	}

	// Reserve space for suffix items on the right.
	suffixWidth := 0
	if diffStr != "" {
		suffixWidth += len(diffStr) + 1 // space + diff
	}
	suffixWidth += len(countBadge)

	// Truncate branch name to fit.
	name := b.Name
	maxName := maxWidth - 2 - suffixWidth // 2 for prefix
	if maxName < 6 {
		maxName = 6
	}
	if len(name) > maxName {
		name = name[:maxName-1] + "…"
	}

	// Style choices.
	nameStyle := lipgloss.NewStyle()
	countStyle := lipgloss.NewStyle()

	if selected {
		nameStyle = nameStyle.Foreground(textColor).Bold(true)
		countStyle = countStyle.Foreground(dimColor).Bold(true)
	} else if b.IsCurrent {
		nameStyle = nameStyle.Foreground(textColor)
		countStyle = countStyle.Foreground(dimColor)
	} else {
		nameStyle = nameStyle.Foreground(textColor)
		countStyle = countStyle.Foreground(dimColor)
	}

	prefix := "  "
	if selected {
		prefix = lipgloss.NewStyle().Foreground(primaryColor).Bold(true).Render("> ")
	}

	line := prefix + nameStyle.Render(name)

	if diffStr != "" {
		addedStr := fmt.Sprintf("+%d", b.LinesAdded)
		removedStr := fmt.Sprintf("-%d", b.LinesRemoved)
		styledDiff := " " +
			lipgloss.NewStyle().Foreground(successColor).Render(addedStr) +
			" " +
			lipgloss.NewStyle().Foreground(dangerColor).Render(removedStr)
		line += styledDiff
	}

	line += countStyle.Render(countBadge)

	return line
}

// listHeight returns the height available for the branch list (excluding border).
func (m *BranchPaneModel) listHeight() int {
	h := m.height - 4 // border top/bottom + some padding
	if h < 5 {
		h = 5
	}
	return h
}

// ensureVisible scrolls to keep the cursor visible.
func (m *BranchPaneModel) ensureVisible() {
	vh := m.listHeight() - 3 // header + blank line + some margin
	if vh < 1 {
		vh = 1
	}
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+vh {
		m.scroll = m.cursor - vh + 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// loadBranches fetches branches from the daemon.
func (m *BranchPaneModel) loadBranches() tea.Cmd {
	client := m.client
	projectDir := m.projectDir
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		branches, err := client.ListBranches(ctx, projectDir)
		if err != nil {
			return branchLoadedMsg{err: err}
		}
		return branchLoadedMsg{branches: branches}
	}
}

// createWorktree asks the daemon to create a worktree for the given branch.
func (m *BranchPaneModel) createWorktree(branch string) tea.Cmd {
	client := m.client
	projectDir := m.projectDir
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := client.CreateWorktree(ctx, daemon.CreateWorktreeRequest{
			ProjectDir: projectDir,
			Branch:     branch,
		})
		return branchWorktreeCreatedMsg{branch: branch, err: err}
	}
}
