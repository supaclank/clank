package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/acksell/clank/internal/daemon"
)

// mergeResultMsg is emitted when the merge overlay completes (success or cancel).
type mergeResultMsg struct {
	merged bool   // true if merge succeeded
	err    error  // non-nil on merge failure
	branch string // the branch that was merged
}

// mergeOverlayModel is a modal overlay that lets the user review and confirm
// a branch merge. It shows branch info, diff stats, a commit log, and an
// editable textarea for the merge commit message.
type mergeOverlayModel struct {
	client     *daemon.Client
	projectDir string
	branch     daemon.BranchInfo

	commitMsg textarea.Model
	merging   bool // true while the merge request is in flight

	width  int
	height int
}

func newMergeOverlay(client *daemon.Client, projectDir string, branch daemon.BranchInfo) mergeOverlayModel {
	// Build default commit message: "Merge branch '<name>'"
	msg := fmt.Sprintf("Merge branch '%s'", branch.Name)

	ta := textarea.New()
	ta.SetValue(msg)
	ta.CharLimit = 4096
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	styles := ta.Styles()
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Focused.Base = lipgloss.NewStyle()
	styles.Blurred.Base = lipgloss.NewStyle()
	ta.SetStyles(styles)
	// Shift+Enter inserts newline; plain Enter triggers merge.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter"),
		key.WithHelp("shift+enter", "newline"),
	)
	ta.Focus()

	return mergeOverlayModel{
		client:     client,
		projectDir: projectDir,
		branch:     branch,
		commitMsg:  ta,
	}
}

func (m *mergeOverlayModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	// Textarea width = overlay inner width minus border/padding.
	innerW := m.overlayInnerWidth()
	if innerW > 0 {
		m.commitMsg.SetWidth(innerW)
	}
}

func (m *mergeOverlayModel) overlayInnerWidth() int {
	w := 56
	if m.width > 0 && m.width < w+6 {
		w = m.width - 6
	}
	if w < 20 {
		w = 20
	}
	return w
}

func (m *mergeOverlayModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case mergeResultMsg:
		// Bubble the result up — the inbox handles this.
		m.merging = false
		return func() tea.Msg { return msg }

	case tea.KeyPressMsg:
		if m.merging {
			return nil // ignore input while merge is in flight
		}

		msg = normalizeKeyCase(msg)

		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			return func() tea.Msg {
				return mergeResultMsg{merged: false, branch: m.branch.Name}
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			commitMsg := strings.TrimSpace(m.commitMsg.Value())
			if commitMsg == "" {
				return nil
			}
			m.merging = true
			return m.doMerge(commitMsg)
		}

		// Forward to textarea.
		var cmd tea.Cmd
		m.commitMsg, cmd = m.commitMsg.Update(msg)
		return cmd
	}

	// Forward non-key messages (e.g. cursor blink) to textarea.
	var cmd tea.Cmd
	m.commitMsg, cmd = m.commitMsg.Update(msg)
	return cmd
}

func (m *mergeOverlayModel) doMerge(commitMsg string) tea.Cmd {
	client := m.client
	projectDir := m.projectDir
	branch := m.branch.Name
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := client.MergeWorktree(ctx, daemon.MergeWorktreeRequest{
			ProjectDir:    projectDir,
			Branch:        branch,
			CommitMessage: commitMsg,
		})
		if err != nil {
			return mergeResultMsg{merged: false, err: err, branch: branch}
		}
		return mergeResultMsg{merged: true, branch: branch}
	}
}

func (m *mergeOverlayModel) View() string {
	var sb strings.Builder
	innerW := m.overlayInnerWidth()

	// Title.
	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(primaryColor).
		Width(innerW).
		Render("Merge Branch")
	sb.WriteString(title)
	sb.WriteString("\n")

	sep := lipgloss.NewStyle().
		Foreground(mutedColor).
		Render(strings.Repeat("─", innerW))
	sb.WriteString(sep)
	sb.WriteString("\n\n")

	// Branch info.
	branchLabel := lipgloss.NewStyle().Foreground(dimColor).Render("Branch: ")
	branchName := lipgloss.NewStyle().Foreground(secondaryColor).Bold(true).Render(m.branch.Name)
	sb.WriteString(branchLabel + branchName)
	sb.WriteString("\n")

	// Commits ahead.
	if m.branch.CommitsAhead > 0 {
		aheadLabel := lipgloss.NewStyle().Foreground(dimColor).Render("Commits: ")
		aheadVal := lipgloss.NewStyle().Foreground(textColor).Render(
			fmt.Sprintf("%d ahead of default", m.branch.CommitsAhead),
		)
		sb.WriteString(aheadLabel + aheadVal)
		sb.WriteString("\n")
	}

	// Diff stats.
	if m.branch.LinesAdded > 0 || m.branch.LinesRemoved > 0 {
		diffLabel := lipgloss.NewStyle().Foreground(dimColor).Render("Changes: ")
		added := lipgloss.NewStyle().Foreground(successColor).Render(fmt.Sprintf("+%d", m.branch.LinesAdded))
		removed := lipgloss.NewStyle().Foreground(dangerColor).Render(fmt.Sprintf("-%d", m.branch.LinesRemoved))
		sb.WriteString(diffLabel + added + " " + removed)
		sb.WriteString("\n")
	}

	sb.WriteString("\n")

	// Commit message label.
	msgLabel := lipgloss.NewStyle().Foreground(dimColor).Render("Commit message:")
	sb.WriteString(msgLabel)
	sb.WriteString("\n")

	// Textarea with border.
	taStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(mutedColor).
		Padding(0, 1).
		Width(innerW)
	sb.WriteString(taStyle.Render(m.commitMsg.View()))
	sb.WriteString("\n\n")

	// Status or hint.
	if m.merging {
		status := lipgloss.NewStyle().Foreground(warningColor).Bold(true).Render("Merging...")
		sb.WriteString(status)
	} else {
		hint := lipgloss.NewStyle().Foreground(dimColor).
			Render("enter: merge  shift+enter: newline  esc: cancel")
		sb.WriteString(hint)
	}

	popup := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(primaryColor).
		Padding(1, 2).
		Render(sb.String())

	return popup
}
