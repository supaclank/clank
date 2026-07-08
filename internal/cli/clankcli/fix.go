package clankcli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// fixPromptTemplate is the builtin "fix" command template, instantiated
// with the shell-quoted command line. The agent runs the command itself
// (backgrounded, monitored) so stdout+stderr and the exit code are
// captured by its own tooling — nothing for the CLI to tee or forward.
// Future user-defined templates generalize this shape (name → template).
const fixPromptTemplate = "Run this command in the background and monitor its output: <command>%s</command>. " +
	"If it fails or reports errors, diagnose the root cause and fix it, then re-run to verify. " +
	"If it succeeds, summarize the output briefly."

func fixCmd() *cobra.Command {
	var backend string
	var projectDir string
	var worktreeBranch string

	cmd := &cobra.Command{
		Use:   "fix <command> [args...]",
		Short: "Have an agent run a command and debug its output",
		Long: `Start an agent session that runs the given command, monitors its
output, and debugs any failure — without opening the TUI:

  clank fix npx expo run:android --device

Flags after the command belong to the command; use '--' to be explicit.
The session runs in the background; run 'clank' to open it.

The daemon is auto-started if not already running.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true

			resolvedDir, err := resolveProjectDir(projectDir)
			if err != nil {
				return err
			}
			bt, err := resolveBackend(backend, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			client, err := ensureDaemon()
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}
			return runPlease(cmd.Context(), client, cmd.OutOrStdout(), pleaseOpts{
				backend:        bt,
				projectDir:     resolvedDir,
				worktreeBranch: worktreeBranch,
				prompt:         fixPrompt(args),
			})
		},
	}

	// Flags after the first positional belong to the child command
	// (docker-run pattern): `clank fix npx expo run:android --device`
	// must not have cobra eat --device.
	cmd.Flags().SetInterspersed(false)

	cmd.Flags().StringVar(&backend, "backend", "", "Backend to use: opencode (default), claude")
	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: current directory)")
	cmd.Flags().StringVar(&worktreeBranch, "worktree", "", "Git branch to work on (creates worktree if needed)")
	cmd.Flags().StringVar(&worktreeBranch, "branch", "", "Git branch to work on (creates worktree if needed)")
	_ = cmd.Flags().MarkHidden("branch") // hidden alias for familiarity

	return cmd
}

// fixPrompt instantiates the fix template with the shell-quoted argv.
func fixPrompt(argv []string) string {
	return fmt.Sprintf(fixPromptTemplate, shellQuoteJoin(argv))
}

// shellQuoteJoin rebuilds a copy-pasteable shell command line from argv.
// The invoking shell already stripped quoting, so args containing spaces
// or shell metacharacters are re-quoted — otherwise the agent would run
// a differently-tokenized command than the user typed.
func shellQuoteJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// shellQuote single-quotes an argument when needed (POSIX-style: an
// embedded single quote closes, escapes, and reopens).
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\r\"'`$&|;()<>*?[]{}~#\\!^") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
