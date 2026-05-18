package clankcli

import "github.com/charmbracelet/lipgloss"

// Shared lipgloss palette for clankcli output. Used by `status`,
// `push`, `pull`, and any future commands that need to show
// success / warning / refusal lines. Aligned so the user sees the
// same colours across commands.
//
// Colours follow standard 16-colour ANSI semantics: 9=red, 10=green,
// 11=yellow, 12=bright-blue. lipgloss strips them automatically when
// stdout isn't a tty (via termenv), so piping to a file / another
// process is safe.
var (
	styleOK          = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true) // green
	styleWarn        = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))            // yellow
	styleErr         = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)  // red
	styleCmdHint     = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))            // bright blue
	styleDim         = lipgloss.NewStyle().Faint(true)
	styleWorktree    = lipgloss.NewStyle().Bold(true)
	styleRemoteOwner = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	styleLocalOwner  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
)
