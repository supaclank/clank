package clankcli

import "github.com/charmbracelet/lipgloss"

// Shared lipgloss palette for clankcli output. Used by `status`,
// `push`, `pull`, and any future commands that need to show
// success / warning / refusal lines. Aligned so the user sees the
// same colours across commands.
//
// Colours follow standard 16-colour ANSI semantics: 8=grey, 9=red,
// 10=green, 11=yellow, 12=bright-blue. lipgloss strips them automatically
// when stdout isn't a tty (via termenv), so piping remains safe.
var (
	styleOK          = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true) // green
	styleWarn        = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))            // yellow
	styleErr         = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)  // red
	styleCmdHint     = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))            // bright blue
	stylePreviewLog  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))             // grey
	styleDim         = lipgloss.NewStyle().Faint(true)
	styleWorktree    = lipgloss.NewStyle().Bold(true)
	styleRemoteOwner = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	styleLocalOwner  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
)
