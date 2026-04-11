// Package daemoncli provides the cobra commands for managing the Clank daemon.
// It is shared between the clank and clankd binaries.
package daemoncli

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/daemon"
	"github.com/acksell/clank/internal/store"
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

	cmd.AddCommand(startCmd, stopCmd, statusCmd, opencodeCmd())
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

		// Open SQLite store for session persistence.
		dir, err := config.Dir()
		if err != nil {
			return fmt.Errorf("config dir: %w", err)
		}
		dbPath := filepath.Join(dir, "clank.db")
		st, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		d.Store = st

		// Open persistent log file. Truncated on each start so it
		// doesn't grow unbounded across daemon restarts.
		logPath := filepath.Join(dir, "daemon.log")
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("open daemon log: %w", err)
		}
		defer logFile.Close()
		// Foreground: write to both stderr (live) and the log file.
		d.SetLogOutput(io.MultiWriter(os.Stderr, logFile))
		// Also redirect the global logger so that subsystems using
		// log.Printf (audio, reconciler) are captured.
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))

		// Wire in backend managers.
		d.BackendManagers[agent.BackendOpenCode] = daemon.NewOpenCodeBackendManager()
		d.BackendManagers[agent.BackendClaudeCode] = daemon.NewClaudeBackendManager()

		return d.Run()
	}

	// Fork a background process. The forked process runs with
	// --foreground, which opens ~/.clank/daemon.log for persistent
	// output. We still redirect stdout/stderr to the log file here
	// so that any early output before the daemon's logger is set up
	// is captured.
	dir, err := config.Dir()
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	logPath := filepath.Join(dir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		logFile.Close()
		return fmt.Errorf("find executable: %w", err)
	}

	bgCmd := exec.Command(exe, "start", "--foreground")
	bgCmd.Stdout = logFile
	bgCmd.Stderr = logFile
	bgCmd.Stdin = nil
	// Start in a new process group so it doesn't get signals from our terminal.
	bgCmd.SysProcAttr = daemonSysProcAttr()

	if err := bgCmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start daemon: %w", err)
	}
	// Child process inherited the fd; close our copy.
	logFile.Close()

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

// opencodeCmd returns the "opencode" subcommand group with its own subcommands.
func opencodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "opencode",
		Short: "OpenCode backend management",
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show running OpenCode servers",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOpenCodeStatus()
		},
	}

	cmd.AddCommand(statusCmd)
	return cmd
}

// runOpenCodeStatus lists running OpenCode server processes and their session counts.
func runOpenCodeStatus() error {
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

	servers, err := client.ListOpenCodeServers(ctx)
	if err != nil {
		fmt.Printf("Daemon process exists (pid=%d) but API is not reachable: %v\n", pid, err)
		return nil
	}

	if len(servers) == 0 {
		fmt.Println("No OpenCode servers running")
		return nil
	}

	fmt.Printf("%d OpenCode server(s):\n", len(servers))
	for _, srv := range servers {
		uptime := time.Since(srv.StartedAt).Truncate(time.Second)
		fmt.Printf("  %s  pid=%-6d  uptime=%-12s  sessions=%-3d  %s\n",
			srv.URL, srv.PID, uptime, srv.SessionCount, srv.ProjectDir)
	}
	return nil
}
