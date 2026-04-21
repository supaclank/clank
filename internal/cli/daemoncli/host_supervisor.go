package daemoncli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	hostclient "github.com/acksell/clank/internal/host/client"
)

// hostHandle owns a clank-host subprocess and the Hub's HTTP client to it.
//
// Phase 1 limitation: no restart-on-crash. If clank-host dies mid-session,
// the Hub keeps a dead client and every subsequent request fails. A
// supervisor with health probes + restart belongs in Phase 2 (or sooner
// if instability bites).
type hostHandle struct {
	cmd        *exec.Cmd
	socketPath string
	client     *hostclient.HTTP
}

// startHost spawns the clank-host binary, waits for its Unix socket to
// become reachable, and returns a configured hostclient.HTTP plus a
// handle for shutdown.
//
// configDir is where the socket file lives (default ~/.clank). logOut
// receives the child's stdout/stderr; pass the daemon's log file so
// host output is interleaved with daemon output.
func startHost(ctx context.Context, configDir string, logOut io.Writer) (*hostHandle, error) {
	binPath, err := resolveHostBinary()
	if err != nil {
		return nil, fmt.Errorf("resolve clank-host binary: %w", err)
	}

	socketPath := filepath.Join(configDir, "host.sock")
	// Remove stale socket from a prior crashed run. clank-host also does
	// this defensively but doing it here surfaces permission errors early.
	_ = os.Remove(socketPath)

	cmd := exec.Command(binPath, "--socket", socketPath)
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	// New process group so terminal signals to clankd don't double-deliver
	// to clank-host; clankd explicitly stops the child during shutdown.
	cmd.SysProcAttr = daemonSysProcAttr()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start clank-host: %w", err)
	}

	// Wait for the socket to be reachable before returning. Poll connect.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := pingSocket(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return nil, fmt.Errorf("clank-host did not become ready at %s within 5s", socketPath)
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	return &hostHandle{
		cmd:        cmd,
		socketPath: socketPath,
		client:     hostclient.NewUnixHTTP(socketPath),
	}, nil
}

// stop signals the clank-host child and waits for it to exit. Always
// removes the socket file. Safe to call multiple times.
func (h *hostHandle) stop() {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.client.Close()
	// SIGTERM-equivalent (os.Interrupt is portable); clank-host's signal
	// handler shuts the HTTP server down gracefully.
	_ = h.cmd.Process.Signal(os.Interrupt)

	done := make(chan struct{})
	go func() {
		// Use cmd.Wait (not Process.Wait) so cmd.ProcessState is
		// populated — needed for tests asserting clean exit and for
		// future stderr/pipe draining.
		_ = h.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// Child didn't exit cleanly — kill it. Avoids lingering sockets
		// blocking the next clankd start.
		_ = h.cmd.Process.Kill()
		<-done
	}
	_ = os.Remove(h.socketPath)
}

// resolveHostBinary finds the clank-host executable. Resolution order:
//  1. CLANK_HOST_BIN env var (dev/test override)
//  2. PATH lookup
//  3. Same directory as the running clankd binary
//
// Errors fast if none found — fallbacks for missing dependencies make
// runtime failures harder to debug (AGENTS.md).
func resolveHostBinary() (string, error) {
	if p := os.Getenv("CLANK_HOST_BIN"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("CLANK_HOST_BIN=%s: %w", p, err)
		}
		return p, nil
	}
	if p, err := exec.LookPath("clank-host"); err == nil {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("os.Executable: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(exe), "clank-host")
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("clank-host not found in PATH or alongside %s", exe)
	}
	return candidate, nil
}

// pingSocket attempts a single connect to the Unix socket. Used to
// detect when clank-host is ready to accept requests.
func pingSocket(path string) error {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
