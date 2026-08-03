package clankcli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/config"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
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
			return runPlease(cmd.Context(), client, cmd.OutOrStdout(), cmd.ErrOrStderr(), pleaseOpts{
				backend:        bt,
				projectDir:     projectDir,
				worktreeBranch: worktreeBranch,
				prompt:         strings.Join(args, " "),
			})
		},
	}

	cmd.Flags().StringVar(&backend, "backend", "", "Backend to use: opencode (default), claude")
	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: current directory)")
	cmd.Flags().StringVar(&worktreeBranch, "worktree", "", "Git branch to work on (creates worktree if needed)")
	cmd.Flags().StringVar(&worktreeBranch, "branch", "", "Git branch to work on (creates worktree if needed)")
	_ = cmd.Flags().MarkHidden("branch") // hidden alias for familiarity

	return cmd
}

type pleaseOpts struct {
	backend        agent.BackendType
	projectDir     string
	worktreeBranch string
	prompt         string
}

// runPlease creates the session (which starts the agent on the initial
// prompt server-side) and returns without watching it: fire-and-forget.
// The session is recorded as the cwd's last session so a bare `clank`
// drops the user straight into it.
func runPlease(ctx context.Context, client *daemonclient.Client, out, errOut io.Writer, opts pleaseOpts) error {
	ctx, cancel := context.WithTimeout(ctx, createSessionTimeout)
	defer cancel()

	req := newStartRequest(opts.backend, opts.projectDir, opts.worktreeBranch, opts.prompt)
	cfg, err := defaultPresetConfig(ctx, client, opts.backend, req.Hostname)
	if err != nil {
		return err
	}
	req.Config = cfg
	info, err := client.Sessions().Create(ctx, req)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	// Synchronous on purpose: the process exits right after, so a
	// goroutine (as the TUI uses) would race process exit.
	if err := config.SetLastSessionForCwd(opts.projectDir, info.ID); err != nil {
		fmt.Fprintf(errOut, "warning: session started but not recorded as last session: %v\n", err)
	}

	fmt.Fprintf(out, "Session %s started: %q — run 'clank' to open it.\n", info.ID, previewPrompt(opts.prompt))
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
