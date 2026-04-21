package tui

// SidebarModel is the navigation sidebar of the inbox layout.
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

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hubclient "github.com/acksell/clank/internal/hub/client"
)

// sidebarWidth is the fixed width of the sidebar (including border).
const sidebarWidth = 30

// branchLoadedMsg carries the result of loading branches from the daemon.
type branchLoadedMsg struct {
	branches []host.BranchInfo
	err      error
}

// branchWorktreeCreatedMsg is sent after a worktree is created for a new branch.
type branchWorktreeCreatedMsg struct {
	branch string
	err    error
}

// branchSessionStatus summarises session states for a single worktree branch.
type branchSessionStatus struct {
	Total    int // Total sessions on this worktree
	Active   int // Visible / in-progress sessions
	Done     int // Sessions marked done
	Archived int // Sessions archived
}

// IsDone returns true when the worktree has sessions and every session is
// either done or archived.
func (s branchSessionStatus) IsDone() bool {
	return s.Total > 0 && s.Active == 0
}

// IsArchived returns true when the worktree has sessions and every session
// is archived (none merely "done").
func (s branchSessionStatus) IsArchived() bool {
	return s.Total > 0 && s.Archived == s.Total
}

// SidebarModel displays local git branches in a navigation sidebar and allows selection.
type SidebarModel struct {
	client *hubclient.Client
	// projectDir is the cwd the inbox was launched from. Kept for display
	// and for non-branch concerns (project filter); branch operations now
	// route through hostname/gitRef instead.
	projectDir string
	hostname   host.Hostname
	gitRef     agent.GitRef

	branches []host.BranchInfo
	cursor   int
	scroll   int

	// Session status per branch (set by the inbox when sessions are loaded).
	sessionStatus map[string]branchSessionStatus // branch name -> session status summary

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

// NewSidebarModel creates a sidebar for the given repo identity.
// projectDir is retained for display purposes only; branch/worktree ops
// are addressed by (hostname, gitRef).
func NewSidebarModel(client *hubclient.Client, hostname host.Hostname, gitRef agent.GitRef, projectDir string) SidebarModel {
	ti := textinput.New()
	ti.Placeholder = "branch-name"
	ti.CharLimit = 128
	ti.Prompt = "+ "
	styles := ti.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(successColor).Bold(true)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(textColor)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(mutedColor)
	ti.SetStyles(styles)

	return SidebarModel{
		client:     client,
		hostname:   hostname,
		gitRef:     gitRef,
		projectDir: projectDir,
		input:      ti,
		cursor:     0, // "All branches" selected by default
	}
}

// Init fetches branches from the daemon.
func (m *SidebarModel) Init() tea.Cmd {
	return m.loadBranches()
}

// SelectedBranch returns the currently selected branch name.
// Empty string means "all branches" (no filter).
func (m *SidebarModel) SelectedBranch() string {
	if m.cursor == 0 || len(m.branches) == 0 {
		return ""
	}
	idx := m.cursor - 1
	if idx >= len(m.branches) {
		return ""
	}
	return m.branches[idx].Name
}

// SelectedWorktreeDir returns the worktree directory path for the currently
// selected entry. Empty string means "all worktrees" (no filter).
func (m *SidebarModel) SelectedWorktreeDir() string {
	if m.cursor == 0 || len(m.branches) == 0 {
		return ""
	}
	idx := m.cursor - 1
	if idx >= len(m.branches) {
		return ""
	}
	return m.branches[idx].WorktreeDir
}

// SelectedBranchInfo returns the full BranchInfo for the currently selected
// entry, or nil if "All" is selected.
func (m *SidebarModel) SelectedBranchInfo() *host.BranchInfo {
	if m.cursor == 0 || len(m.branches) == 0 {
		return nil
	}
	idx := m.cursor - 1
	if idx >= len(m.branches) {
		return nil
	}
	return &m.branches[idx]
}

// SetFocused sets whether the sidebar has keyboard focus.
func (m *SidebarModel) SetFocused(focused bool) {
	m.focused = focused
}

// Focused returns whether the sidebar has keyboard focus.
func (m *SidebarModel) Focused() bool {
	return m.focused
}

// SetSize sets the sidebar dimensions.
func (m *SidebarModel) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// SetSessionStatus updates the per-branch session status displayed in the sidebar.
func (m *SidebarModel) SetSessionStatus(status map[string]branchSessionStatus) {
	m.sessionStatus = status
}

// WorktreeDirToBranch returns a map from worktree directory path to branch name
// for all branches that have an active worktree. The inbox uses this to count
// sessions by matching SessionInfo.ProjectDir against worktree paths.
func (m *SidebarModel) WorktreeDirToBranch() map[string]string {
	result := make(map[string]string, len(m.branches))
	for _, b := range m.branches {
		if b.WorktreeDir != "" {
			result[b.WorktreeDir] = b.Name
		}
	}
	return result
}

// Update handles messages for the sidebar.
func (m *SidebarModel) Update(msg tea.Msg) tea.Cmd {
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
func (m *SidebarModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
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
func (m *SidebarModel) updateCreating(msg tea.Msg) tea.Cmd {
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

// View renders the sidebar.
func (m *SidebarModel) View() string {
	w := m.width
	if w <= 0 {
		w = sidebarWidth
	}

	// Content width is sidebar width minus border (2) minus padding (2).
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
func (m *SidebarModel) renderBranch(b host.BranchInfo, idx, maxWidth int) string {
	selected := m.cursor == idx && m.focused

	// Diff stat string: "+123 -45" or empty for the default branch.
	var diffStr string
	if b.LinesAdded > 0 || b.LinesRemoved > 0 {
		diffStr = fmt.Sprintf("+%d -%d", b.LinesAdded, b.LinesRemoved)
	}

	// Session status badge.
	status := m.sessionStatus[b.Name]
	countBadge := ""
	badgeColor := dimColor
	if status.Total > 0 {
		if status.IsArchived() {
			countBadge = fmt.Sprintf(" (%d)", status.Total)
			badgeColor = mutedColor
		} else if status.IsDone() {
			countBadge = fmt.Sprintf(" (%d)", status.Total)
			badgeColor = successColor
		} else {
			countBadge = fmt.Sprintf(" (%d)", status.Active)
		}
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

	if selected {
		nameStyle = nameStyle.Foreground(textColor).Bold(true)
	} else if status.IsArchived() {
		nameStyle = nameStyle.Foreground(mutedColor)
	} else if status.IsDone() {
		nameStyle = nameStyle.Foreground(dimColor)
	} else {
		nameStyle = nameStyle.Foreground(textColor)
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

	line += lipgloss.NewStyle().Foreground(badgeColor).Render(countBadge)

	return line
}

// listHeight returns the height available for the branch list (excluding border).
func (m *SidebarModel) listHeight() int {
	h := m.height - 4 // border top/bottom + some padding
	if h < 5 {
		h = 5
	}
	return h
}

// ensureVisible scrolls to keep the cursor visible.
func (m *SidebarModel) ensureVisible() {
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

// loadBranches fetches branches from the daemon for this sidebar's repo.
func (m *SidebarModel) loadBranches() tea.Cmd {
	client := m.client
	hostname := m.hostname
	gitRef := m.gitRef
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		branches, err := client.Host(hostname).ListBranches(ctx, gitRef)
		if err != nil {
			return branchLoadedMsg{err: err}
		}
		return branchLoadedMsg{branches: branches}
	}
}

// createWorktree asks the daemon to create a worktree for the given branch.
func (m *SidebarModel) createWorktree(branch string) tea.Cmd {
	client := m.client
	hostname := m.hostname
	gitRef := m.gitRef
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := client.Host(hostname).ResolveWorktree(ctx, gitRef, branch)
		return branchWorktreeCreatedMsg{branch: branch, err: err}
	}
}
