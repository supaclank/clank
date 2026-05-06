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
	daemonclient "github.com/acksell/clank/internal/daemonclient"
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
			listen, _ := cmd.Flags().GetString("listen")
			publicBaseURL, _ := cmd.Flags().GetString("public-base-url")
			return RunStart(foreground, ServerOptions{
				Listen:        listen,
				PublicBaseURL: publicBaseURL,
			})
		},
	}
	startCmd.Flags().Bool("foreground", false, "Run in foreground (don't daemonize)")
	startCmd.Flags().String("listen", "", "Listener address override, e.g. tcp://0.0.0.0:7878. Empty (default) = Unix socket. TCP mode requires preferences.remote_hub.auth_token to be set and authorizes inbound calls with that bearer token.")
	startCmd.Flags().String("public-base-url", "", "Externally-reachable base URL of this hub. Used by TCP-mode hubs to tell sandboxes where to fetch git mirrors from.")

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

// ServerOptions configures the listener for the clankd Hub. Empty Listen
// means default Unix socket mode; "tcp://addr:port" enables TCP mode with
// bearer-token auth. PublicBaseURL is the externally-reachable URL of
// the hub, used in TCP mode to tell sandboxes where to fetch synced data.
type ServerOptions struct {
	Listen        string
	PublicBaseURL string
}

// RunStart starts the daemon, either in foreground or as a background process.
// Exported so that ensureDaemon in the clank binary can call it directly.
func RunStart(foreground bool, opts ServerOptions) error {
	running, pid, err := daemonclient.IsRunning()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if running {
		fmt.Printf("Daemon already running (pid=%d)\n", pid)
		return nil
	}

	if foreground {
		// PR 3 wiring: gateway + provisioner replaces the legacy hub
		// stack. The daemon picks one provisioner based on
		// preferences.default_launch_host_provider (default "local")
		// and mounts the gateway on opts.Listen. Every request flows
		// gateway → user's host (subprocess for "local", Daytona
		// sandbox or Fly Sprite for the cloud variants).

		// Open SQLite store for the host registry. Session metadata
		// now lives on the host's own host.db (managed by clank-host
		// inside its --data-dir); this store holds only the hosts
		// table the provisioner uses for cross-restart sandbox
		// identity (PR 1).
		dir, err := config.Dir()
		if err != nil {
			return fmt.Errorf("config dir: %w", err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir config dir: %w", err)
		}
		dbPath := filepath.Join(dir, "clank.db")
		st, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		defer st.Close()

		// Open persistent log file. Truncated on each start so it
		// doesn't grow unbounded across daemon restarts.
		logPath := filepath.Join(dir, "daemon.log")
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("open daemon log: %w", err)
		}
		defer logFile.Close()
		// Redirect the global logger so subsystems using log.Printf
		// land in the same file.
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))

		prov, cleanup, err := buildProvisioner(opts, st)
		if err != nil {
			return fmt.Errorf("build provisioner: %w", err)
		}
		if cleanup != nil {
			defer cleanup()
		}

		return runGatewayServer(prov, opts)
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

	bgArgs := []string{"start", "--foreground"}
	if opts.Listen != "" {
		bgArgs = append(bgArgs, "--listen", opts.Listen)
	}
	if opts.PublicBaseURL != "" {
		bgArgs = append(bgArgs, "--public-base-url", opts.PublicBaseURL)
	}
	bgCmd := exec.Command(exe, bgArgs...)
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

	// Wait briefly for the daemon to be reachable. Always probe the
	// local socket here — we just spawned a local daemon. NewLocalClient
	// (rather than NewDefaultClient) keeps this immune to ActiveHub
	// flipping the user-facing transport to a remote hub.
	client, err := daemonclient.NewLocalClient()
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
	running, pid, err := daemonclient.IsRunning()
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
		stillRunning, _, _ := daemonclient.IsRunning()
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
	running, pid, err := daemonclient.IsRunning()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if !running {
		fmt.Println("Daemon is not running")
		return nil
	}

	// `clankd status` reports on the local daemon — IsRunning already
	// checked the local PID file, so the client must target the local
	// socket too even when ActiveHub points at a remote hub.
	client, err := daemonclient.NewLocalClient()
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
			project := agent.RepoDisplayName(s.GitRef)
			fmt.Printf("  [%s] %-8s %-12s %s\n", s.ID[:8], s.Status, project, prompt)
		}
	}
	return nil
}
