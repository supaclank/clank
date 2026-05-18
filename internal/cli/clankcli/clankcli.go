// Package clankcli provides the root cobra command for the clank binary.
package clankcli

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/host"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/tui"
)

// Command returns the root cobra command for the clank binary with all subcommands.
func Command() *cobra.Command {
	root := &cobra.Command{
		Use:   "clank",
		Short: "AI-powered coding session manager",
		Long:  "Clank manages your coding agent sessions and helps you track what's in flight.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runInbox()
		},
	}

	root.AddCommand(
		codeCmd(),
		inboxCmd(),
		pushCmd(),
		pullCmd(),
		statusCmd(),
		remoteCmd(),
		loginCmd(),
	)

	return root
}

// --- clank code ---

func codeCmd() *cobra.Command {
	var backend string
	var projectDir string
	var ticketID string
	var worktreeBranch string

	cmd := &cobra.Command{
		Use:   "code [prompt]",
		Short: "Launch a new coding agent session",
		Long: `Launch a new coding agent session managed by the Clank daemon.

If a prompt is provided, the session starts immediately and opens the
session detail TUI. Without a prompt, opens the inbox TUI.

The daemon is auto-started if not already running.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			// Determine prompt.
			prompt := strings.Join(args, " ")
			if prompt == "" {
				// No prompt — open composing view standalone.
				return runComposing(projectDir, worktreeBranch)
			}

			// Determine project directory. Resolve to an absolute path
			// so that GitRef.LocalPath is stable regardless of where
			// the daemon happens to be running from when it consumes
			// the request.
			if projectDir == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
				projectDir = cwd
			}
			absProjectDir, err := filepath.Abs(projectDir)
			if err != nil {
				return fmt.Errorf("resolve project dir %q: %w", projectDir, err)
			}
			projectDir = absProjectDir

			// Resolve backend type. Precedence:
			//   1. --backend flag (explicit override)
			//   2. preferences.json default_backend
			//   3. agent.DefaultBackend
			var bt agent.BackendType
			if backend != "" {
				parsed, err := agent.ParseBackend(backend)
				if err != nil {
					return err
				}
				bt = parsed
			} else {
				prefs, _ := config.LoadPreferences()
				resolved, err := agent.ResolveBackendPreference(prefs.DefaultBackend)
				if err != nil {
					// Corrupt preference: warn but proceed with default.
					fmt.Fprintf(os.Stderr, "warning: %v; using %s\n", err, resolved)
				}
				bt = resolved
			}

			// Ensure daemon is running.
			client, err := ensureDaemon()
			if err != nil {
				return fmt.Errorf("daemon: %w", err)
			}

			// Subscribe to SSE BEFORE creating the session so we don't miss
			// events emitted during session startup.
			sseCtx, sseCancel := context.WithCancel(context.Background())
			events, err := client.Sessions().Subscribe(sseCtx)
			if err != nil {
				sseCancel()
				return fmt.Errorf("subscribe events: %w", err)
			}

			// Create the session. Generous timeout because Daytona-
			// launched sessions block here while the cloud hub
			// provisions a sandbox (image build + boot + readiness
			// probe routinely takes 1-3 minutes for a cold start).
			// Local-only sessions return in well under a second; the
			// upper bound only matters for slow paths.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			worktreeID, _ := agent.ReadLocalWorktreeID(projectDir) // empty if `clank sync push` hasn't been run

			info, err := client.Sessions().Create(ctx, agent.StartRequest{
				Backend:  bt,
				Hostname: host.HostLocal,
				GitRef: agent.GitRef{
					LocalPath:      projectDir,
					WorktreeID:     worktreeID,
					WorktreeBranch: worktreeBranch,
				},
				Prompt:   prompt,
				TicketID: ticketID,
			})
			if err != nil {
				sseCancel()
				return fmt.Errorf("create session: %w", err)
			}

			// Open session detail TUI with pre-connected event channel.
			model := tui.NewSessionViewModel(client, info.ID)
			model.SetStandalone(true)
			model.SetEventChannel(events, sseCancel)
			cleanup := redirectLogToFile()
			defer cleanup()
			p := tea.NewProgram(model)
			_, err = p.Run()
			return err
		},
	}

	cmd.Flags().StringVar(&backend, "backend", "", "Backend to use: opencode (default), claude")
	cmd.Flags().StringVar(&projectDir, "project", "", "Project directory (default: current directory)")
	cmd.Flags().StringVar(&ticketID, "ticket", "", "Link to backlog ticket ID")
	cmd.Flags().StringVar(&worktreeBranch, "worktree", "", "Git branch to work on (creates worktree if needed)")
	cmd.Flags().StringVar(&worktreeBranch, "branch", "", "Git branch to work on (creates worktree if needed)")
	_ = cmd.Flags().MarkHidden("branch") // hidden alias for familiarity

	return cmd
}

func inboxCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inbox",
		Short: "Open the agent session inbox",
		Long:  "View and manage daemon-managed coding agent sessions in an interactive TUI.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runInbox()
		},
	}
}

// runInbox opens the inbox TUI. Ensures the daemon is running first.
func runInbox() error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	model := tui.NewInboxModel(client)
	cleanup := redirectLogToFile()
	defer cleanup()
	p := tea.NewProgram(model)
	_, err = p.Run()
	return err
}

// runComposing opens the composing view standalone (not inside inbox).
// The user types their first prompt and the session is created on send.
func runComposing(projectDir, worktreeBranch string) error {
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
		projectDir = cwd
	}
	absProjectDir, err := filepath.Abs(projectDir)
	if err != nil {
		return fmt.Errorf("resolve project dir %q: %w", projectDir, err)
	}
	projectDir = absProjectDir

	model := tui.NewSessionViewComposing(client, projectDir)
	model.SetWorktreeBranch(worktreeBranch)
	model.SetStandalone(true)
	cleanup := redirectLogToFile()
	defer cleanup()
	p := tea.NewProgram(model)
	_, err = p.Run()
	return err
}

// redirectLogToFile sends the default logger's output to a PID-scoped
// file so that log.Printf calls from audio goroutines and other
// background work don't overwrite the TUI (stderr is not captured by
// Bubble Tea's alt screen). Returns a cleanup function that should be
// deferred.
func redirectLogToFile() func() {
	path := fmt.Sprintf("/tmp/clank-tui-%d.log", os.Getpid())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		// Non-fatal: if we can't open the file, just leave stderr as-is.
		return func() {}
	}
	log.SetOutput(f)
	return func() {
		log.SetOutput(os.Stderr)
		f.Close()
	}
}

// ensureDaemon makes sure the local clankd is running, starting it
// if needed. Returns a connected Unix-socket client.
//
// `clank` only ever talks to the local daemon: per-session ops to
// remote-owned worktrees are proxied transparently through the local
// gateway. The old ActiveHub toggle (laptop client → remote daemon)
// is gone; remote daemons are the responsibility of `clank push`/
// `clank pull` flows that explicitly use NewRemoteClient.
func ensureDaemon() (*daemonclient.Client, error) {
	running, _, err := daemonclient.IsRunning()
	if err != nil {
		return nil, err
	}

	if !running {
		fmt.Println("Starting daemon...")
		if err := spawnLocalDaemon(); err != nil {
			return nil, fmt.Errorf("start daemon: %w", err)
		}
	}

	client, err := daemonclient.NewDefaultClient()
	if err != nil {
		return nil, err
	}

	// Verify reachable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return nil, fmt.Errorf("daemon not reachable: %w", err)
	}

	return client, nil
}
