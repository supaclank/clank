package daemoncli

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	hub "github.com/acksell/clank/internal/hub"
	hubclient "github.com/acksell/clank/internal/hub/client"
	hubmux "github.com/acksell/clank/internal/hub/mux"
	"github.com/acksell/clank/internal/socketutil"
)

// runHubServer is the production driver for hub.Service: it owns the
// listener, the PID file, and the signal-to-Stop translation. The
// Hub itself never touches the filesystem for these artifacts — keeping
// the orchestration concerns out of the Hub plane.
//
// Lifecycle:
//  1. Open Unix listener on hubclient.SocketPath() (clearing any stale
//     socket file first).
//  2. chmod 0600 so only the user can connect.
//  3. Write hubclient.PIDPath() with our PID.
//  4. Install SIGINT/SIGTERM handler that calls s.Stop().
//  5. Block on s.Run(listener).
//  6. On return, remove the PID file and socket file.
func runHubServer(s *hub.Service) error {
	sockPath, err := hubclient.SocketPath()
	if err != nil {
		return fmt.Errorf("socket path: %w", err)
	}
	pidPath, err := hubclient.PIDPath()
	if err != nil {
		return fmt.Errorf("pid path: %w", err)
	}

	// Clear any stale socket left behind by a previous crash. IsRunning
	// already removes stale sockets when it sees a dead PID, but we
	// still need to handle the case where the PID file was hand-removed.
	// RemoveStale refuses to delete non-socket files so a misconfigured
	// path cannot clobber user data. Probe the socket first: if a live
	// daemon is answering, refuse to start so we don't unlink an active
	// peer's listener.
	if conn, dialErr := net.DialTimeout("unix", sockPath, 200*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("clankd already running on %s", sockPath)
	}
	if err := socketutil.RemoveStale(sockPath); err != nil {
		return err
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		listener.Close()
		_ = socketutil.RemoveStale(sockPath)
		return fmt.Errorf("chmod socket: %w", err)
	}

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		listener.Close()
		_ = socketutil.RemoveStale(sockPath)
		return fmt.Errorf("write PID file: %w", err)
	}

	// Best-effort cleanup of on-disk artifacts; both removes are no-ops
	// if shutdown already raced and removed them.
	defer func() {
		if err := socketutil.RemoveStale(sockPath); err != nil {
			log.Printf("socket cleanup: %v", err)
		}
		os.Remove(pidPath)
	}()

	// Translate SIGINT/SIGTERM into a Stop request. Run owns the actual
	// drain via s.ctx; we just trip it. The done channel ensures the
	// signal goroutine exits when Run returns even without a signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case sig := <-sigCh:
			log.Printf("received signal %v, shutting down", sig)
			s.Stop()
		case <-done:
		}
	}()

	return s.Run(listener, hubmux.New(s, nil).Handler())
}
