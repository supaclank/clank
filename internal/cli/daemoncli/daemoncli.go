// Package daemoncli provides the cobra commands for managing the Clank daemon.
// It is shared between the clank and clankd binaries.
package daemoncli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/daemon"
)

// Command returns the root cobra command for the clankd binary with start/stop/status subcommands.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clankd",
		Short: "Clank daemon manager",
		Long:  "clankd manages the Clank background daemon that runs coding agent sessions.",
	}

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the background daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			foreground, _ := cmd.Flags().GetBool("foreground")
			return RunStart(foreground)
		},
	}
	startCmd.Flags().Bool("foreground", false, "Run in foreground (don't daemonize)")

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the background daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop()
		},
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon status and managed sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus()
		},
	}

	cmd.AddCommand(startCmd, stopCmd, statusCmd)
	return cmd
}

// RunStart starts the daemon, either in foreground or as a background process.
// Exported so that ensureDaemon in the clank binary can call it directly.
func RunStart(foreground bool) error {
	running, pid, err := daemon.IsRunning()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if running {
		fmt.Printf("Daemon already running (pid=%d)\n", pid)
		return nil
	}

	if foreground {
		// Run in foreground — useful for debugging.
		d, err := daemon.New()
		if err != nil {
			return err
		}
		// Wire in real backend factory.
		factory := daemon.NewDefaultBackendFactory()
		d.BackendFactory = factory.Create
		d.AgentLister = factory.ListAgents
		d.OnShutdown = factory.StopAll
		return d.Run()
	}

	// Fork a background process.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	bgCmd := exec.Command(exe, "start", "--foreground")
	bgCmd.Stdout = nil
	bgCmd.Stderr = nil
	bgCmd.Stdin = nil
	// Start in a new process group so it doesn't get signals from our terminal.
	bgCmd.SysProcAttr = daemonSysProcAttr()

	if err := bgCmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Wait briefly for the daemon to be reachable.
	client, err := daemon.NewDefaultClient()
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for {
		if err := client.Ping(ctx); err == nil {
			fmt.Printf("Daemon started (pid=%d)\n", bgCmd.Process.Pid)
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Printf("Daemon process started (pid=%d) but not yet reachable\n", bgCmd.Process.Pid)
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// runStop sends SIGTERM to the running daemon.
func runStop() error {
	running, pid, err := daemon.IsRunning()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if !running {
		fmt.Println("Daemon is not running")
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	if err := proc.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("signal daemon (pid=%d): %w", pid, err)
	}

	// Wait for it to exit.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for {
		stillRunning, _, _ := daemon.IsRunning()
		if !stillRunning {
			fmt.Printf("Daemon stopped (was pid=%d)\n", pid)
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Printf("Daemon may still be shutting down (pid=%d)\n", pid)
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// runStatus shows daemon info and managed sessions.
func runStatus() error {
	running, pid, err := daemon.IsRunning()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if !running {
		fmt.Println("Daemon is not running")
		return nil
	}

	client, err := daemon.NewDefaultClient()
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := client.Status(ctx)
	if err != nil {
		// Daemon process exists but API not reachable.
		fmt.Printf("Daemon process exists (pid=%d) but API is not reachable: %v\n", pid, err)
		return nil
	}

	fmt.Printf("Daemon running (pid=%d, uptime=%s)\n", status.PID, status.Uptime)
	if len(status.Sessions) == 0 {
		fmt.Println("No managed sessions")
	} else {
		fmt.Printf("\n%d managed session(s):\n", len(status.Sessions))
		for _, s := range status.Sessions {
			prompt := s.Prompt
			if len(prompt) > 50 {
				prompt = prompt[:47] + "..."
			}
			fmt.Printf("  [%s] %-8s %-12s %s\n", s.ID[:8], s.Status, s.ProjectName, prompt)
		}
	}
	return nil
}
