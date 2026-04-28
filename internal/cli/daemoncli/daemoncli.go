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
	locallauncher "github.com/acksell/clank/internal/host/launcher/local"
	hub "github.com/acksell/clank/internal/hub"
	hubclient "github.com/acksell/clank/internal/hub/client"
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
	running, pid, err := hubclient.IsRunning()
	if err != nil {
		return fmt.Errorf("check daemon: %w", err)
	}
	if running {
		fmt.Printf("Daemon already running (pid=%d)\n", pid)
		return nil
	}

	if foreground {
		// Run in foreground — useful for debugging.
		d := hub.New()

		// Open SQLite store for session persistence.
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

		// Spawn the clank-host subprocess. The Hub (this daemon)
		// communicates with the Host via a Unix socket; backend managers
		// and SessionBackends live in clank-host's address space.
		hh, err := startHost(context.Background(), dir, io.MultiWriter(os.Stderr, logFile))
		if err != nil {
			return fmt.Errorf("start clank-host: %w", err)
		}
		// Stop the child after daemon.Run returns (graceful or not).
		defer hh.stop()

		d.SetHostClient(hh.client)

		// Start the laptop-side sync agent if the user has configured a
		// remote hub. The agent owns its own goroutine; Stop() blocks
		// until the loop exits, so we defer it before runHubServer takes
		// over. Errors here are fatal: a misconfigured agent should not
		// silently fail.
		stopAgent, err := maybeStartSyncAgent(context.Background(), d.Store)
		if err != nil {
			return fmt.Errorf("start sync agent: %w", err)
		}
		if stopAgent != nil {
			defer stopAgent()
		}

		// In TCP/cloud-hub mode the local-stub launcher clones from the
		// hub's own mirror so spawned hosts behave like a real sandbox.
		launcherOpts := locallauncher.Options{}
		if opts.Listen != "" && opts.PublicBaseURL != "" {
			launcherOpts.GitSyncSource = opts.PublicBaseURL
			prefs, perr := config.LoadPreferences()
			if perr != nil {
				log.Printf("local launcher: preferences load failed (%v) — GitSyncToken will be empty", perr)
			} else if prefs.RemoteHub != nil {
				launcherOpts.GitSyncToken = prefs.RemoteHub.AuthToken
			}
		}
		localLauncher := locallauncher.New(launcherOpts, nil)
		d.SetHostLauncher("local-stub", localLauncher)
		defer localLauncher.Stop()
		// Validate default_launch_host_provider against actually-registered launchers.
		registeredLaunchers := map[string]bool{"local-stub": true}

		// Daytona launcher: TCP mode only, preferences.daytona.api_key required.
		// Misconfiguration is logged but non-fatal.
		if opts.Listen != "" {
			if dl, err := buildDaytonaLauncher(opts); err != nil {
				log.Printf("daytona launcher: not registered: %v", err)
			} else if dl != nil {
				d.SetHostLauncher("daytona", dl)
				registeredLaunchers["daytona"] = true
				defer dl.Stop()
			}
		}

		// Apply the configured default launch host only if its launcher is registered.
		if prefs, err := config.LoadPreferences(); err == nil && prefs.DefaultLaunchHostProvider != "" {
			if registeredLaunchers[prefs.DefaultLaunchHostProvider] {
				d.SetDefaultLaunchHost(&agent.LaunchHostSpec{Provider: prefs.DefaultLaunchHostProvider})
				log.Printf("default launch host: %s (applied to sessions without an explicit LaunchHost)", prefs.DefaultLaunchHostProvider)
			} else {
				log.Printf("default launch host %q ignored: launcher not registered", prefs.DefaultLaunchHostProvider)
			}
		}

		return runHubServer(d, opts)
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
	client, err := hubclient.NewLocalClient()
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
	running, pid, err := hubclient.IsRunning()
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
		stillRunning, _, _ := hubclient.IsRunning()
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
	running, pid, err := hubclient.IsRunning()
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
	client, err := hubclient.NewLocalClient()
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
