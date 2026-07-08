package clankcli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
)

// createSessionTimeout bounds Sessions().Create. Generous because
// cloud-launched sessions block here while a sandbox provisions (cold
// start routinely takes 1-3 minutes); local sessions return in well
// under a second.
const createSessionTimeout = 5 * time.Minute

// promptPreviewMaxLen caps the prompt echo in the confirmation line.
const promptPreviewMaxLen = 60

func pleaseCmd() *cobra.Command {
	var backend string
	var projectDir string
	var worktreeBranch string
	var toPicker bool
	var sessionID string

	cmd := &cobra.Command{
		Use:     "please <prompt...>",
		Aliases: []string{"pls", "p"},
		Short:   "Start an agent session from a prompt, without opening the TUI",
		Long: `Start a coding agent session from a prompt and return to the shell.

The prompt is the joined arguments — quotes are optional:

  clank please install a release build to my phone

The session runs in the background; run 'clank' to open it. For a
prompt that starts with '-', separate it with '--':

  clank please -- --verbose is not doing anything, why?

To follow up on an existing session instead of starting a new one,
pick it interactively with --to, or target it directly with
--session <id>.

The daemon is auto-started if not already running.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true

			projectDir, err := resolveProjectDir(projectDir)
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
			target, err := resolveTargetSession(cmd.Context(), client, projectDir, toPicker, sessionID)
			if err != nil {
				return dropPickCanceled(err)
			}
			return runPrompt(cmd.Context(), client, cmd.OutOrStdout(), cmd.ErrOrStderr(), promptOpts{
				backend:         bt,
				projectDir:      projectDir,
				worktreeBranch:  worktreeBranch,
				prompt:          strings.Join(args, " "),
				targetSessionID: target,
			})
		},
	}

	cmd.Flags().StringVar(&backend, "backend", "", "Backend to use: opencode (default), claude")
	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: current directory)")
	cmd.Flags().StringVar(&worktreeBranch, "worktree", "", "Git branch to work on (creates worktree if needed)")
	cmd.Flags().StringVar(&worktreeBranch, "branch", "", "Git branch to work on (creates worktree if needed)")
	_ = cmd.Flags().MarkHidden("branch") // hidden alias for familiarity
	addTargetSessionFlags(cmd, &toPicker, &sessionID)

	return cmd
}

// addTargetSessionFlags registers the existing-session targeting flags
// shared by please and fix.
func addTargetSessionFlags(cmd *cobra.Command, toPicker *bool, sessionID *string) {
	cmd.Flags().BoolVar(toPicker, "to", false, "Pick an existing session to send the prompt to")
	cmd.Flags().StringVar(sessionID, "session", "", "Send the prompt to this existing session id")
	cmd.MarkFlagsMutuallyExclusive("to", "session")
}

type promptOpts struct {
	backend        agent.BackendType
	projectDir     string
	worktreeBranch string
	prompt         string
	// targetSessionID, when set, sends the prompt to that existing
	// session instead of creating a new one.
	targetSessionID string
}

// runPrompt delivers the prompt headlessly and returns without watching:
// fire-and-forget. Default is a new session (creation dispatches the
// initial prompt server-side); with targetSessionID set it is a
// follow-up message to that session. Either way the session is recorded
// as the cwd's last session so a bare `clank` drops the user into it.
func runPrompt(ctx context.Context, client *daemonclient.Client, out, errOut io.Writer, opts promptOpts) error {
	ctx, cancel := context.WithTimeout(ctx, createSessionTimeout)
	defer cancel()

	sessionID := opts.targetSessionID
	if sessionID == "" {
		info, err := client.Sessions().Create(ctx, newStartRequest(opts.backend, opts.projectDir, opts.worktreeBranch, "", opts.prompt))
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		sessionID = info.ID
	} else {
		if err := client.Session(sessionID).Send(ctx, agent.SendMessageOpts{Text: opts.prompt}); err != nil {
			return fmt.Errorf("send to session %s: %w", sessionID, err)
		}
	}

	// Synchronous on purpose: the process exits right after, so a
	// goroutine (as the TUI uses) would race process exit.
	if err := config.SetLastSessionForCwd(opts.projectDir, sessionID); err != nil {
		fmt.Fprintf(errOut, "warning: prompt delivered but session not recorded as last session: %v\n", err)
	}

	if opts.targetSessionID != "" {
		fmt.Fprintf(out, "Sent to session %s: %q — run 'clank' to open it.\n", sessionID, previewPrompt(opts.prompt))
		return nil
	}
	fmt.Fprintf(out, "Session %s started: %q — run 'clank' to open it.\n", sessionID, previewPrompt(opts.prompt))
	return nil
}

// previewPrompt truncates the prompt for the one-line confirmation echo.
func previewPrompt(prompt string) string {
	runes := []rune(prompt)
	if len(runes) <= promptPreviewMaxLen {
		return prompt
	}
	return string(runes[:promptPreviewMaxLen]) + "…"
}
