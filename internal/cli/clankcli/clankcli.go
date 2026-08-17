// Package clankcli provides the root cobra command for the clank binary.
package clankcli

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/tui"
	"github.com/supaclank/clank/internal/version"
)

// Command returns the root cobra command for the clank binary with all subcommands.
func Command() *cobra.Command {
	root := &cobra.Command{
		Use:   "clank",
		Short: "AI-powered coding session manager",
		Long:  "Clank manages your coding agent sessions and helps you track what's in flight.",
		// Enables --version alongside the explicit `version` subcommand.
		Version: version.String(),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runInbox()
		},
	}

	// Hide cobra's auto-generated `completion` command from help; shell
	// completion still works for users who invoke it explicitly.
	root.CompletionOptions.HiddenDefaultCmd = true

	root.AddCommand(
		pleaseCmd(),
		fixCmd(),
		previewCmd(),
		pairCmd(),
		inboxCmd(),
		remoteCmd(),
		loginCmd(),
		logoutCmd(),
		githubCmd(),
		connectCmd(),
		versionCmd(),
	)

	return root
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
// gateway.
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
