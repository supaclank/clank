package daemoncli

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/config"
	hub "github.com/acksell/clank/internal/hub"
	hubclient "github.com/acksell/clank/internal/hub/client"
	hubmux "github.com/acksell/clank/internal/hub/mux"
	"github.com/acksell/clank/internal/socketutil"
	clanksync "github.com/acksell/clank/internal/sync"
)

// runHubServer is the production driver for hub.Service: it owns the
// listener, the PID file, and the signal-to-Stop translation. The
// Hub itself never touches the filesystem for these artifacts — keeping
// the orchestration concerns out of the Hub plane.
//
// Two listening modes:
//   - Unix socket (default): listens on hubclient.SocketPath(), chmod 0600.
//     No HTTP auth — file mode is the gate. Used by laptop hubs talking to
//     local TUI/CLI clients.
//   - TCP (opts.Listen = "tcp://addr:port"): listens on TCP, wraps the
//     handler with a static bearer-token middleware comparing against
//     Preferences.RemoteHub.AuthToken. Used by "remote hubs" that accept
//     hub-to-hub sync calls and external clients (mobile).
//
// Both modes write the PID file at hubclient.PIDPath() so `clankd stop`
// works uniformly.
func runHubServer(s *hub.Service, opts ServerOptions) error {
	pidPath, err := hubclient.PIDPath()
	if err != nil {
		return fmt.Errorf("pid path: %w", err)
	}

	// Resolve the bearer token before binding the listener so a
	// misconfigured TCP launch fails fast without leaving a stray socket.
	mux := hubmux.New(s, nil)
	if opts.Listen != "" {
		recv, err := buildSyncReceiver(s)
		if err != nil {
			return err
		}
		mux = mux.WithSync(recv)
	}
	handler := mux.Handler()
	if opts.Listen != "" {
		token, err := tcpAuthToken()
		if err != nil {
			return err
		}
		handler = bearerAuthMiddleware(handler, token)
	}

	listener, cleanup, err := openHubListener(opts)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		listener.Close()
		return fmt.Errorf("write PID file: %w", err)
	}
	defer os.Remove(pidPath)

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

	return s.Run(listener, handler)
}

// openHubListener creates the appropriate listener for the configured
// mode and returns a cleanup func that removes any on-disk artifacts.
func openHubListener(opts ServerOptions) (net.Listener, func(), error) {
	if opts.Listen == "" {
		return openUnixListener()
	}
	addr, err := parseTCPListen(opts.Listen)
	if err != nil {
		return nil, nil, err
	}
	return openTCPListener(addr)
}

func openUnixListener() (net.Listener, func(), error) {
	sockPath, err := hubclient.SocketPath()
	if err != nil {
		return nil, nil, fmt.Errorf("socket path: %w", err)
	}
	// Refuse to start if a live daemon is answering on the socket.
	// RemoveStale is safe (it refuses non-sockets) but probing first
	// avoids unlinking an active peer's listener.
	if conn, dialErr := net.DialTimeout("unix", sockPath, 200*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("clankd already running on %s", sockPath)
	}
	if err := socketutil.RemoveStale(sockPath); err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		listener.Close()
		_ = socketutil.RemoveStale(sockPath)
		return nil, nil, fmt.Errorf("chmod socket: %w", err)
	}
	cleanup := func() {
		if err := socketutil.RemoveStale(sockPath); err != nil {
			log.Printf("socket cleanup: %v", err)
		}
	}
	return listener, cleanup, nil
}

func openTCPListener(addr string) (net.Listener, func(), error) {
	if conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("address already in use: %s", addr)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen tcp %s: %w", addr, err)
	}
	return listener, func() {}, nil
}

// parseTCPListen accepts "tcp://host:port" or "host:port" and returns the
// host:port suitable for net.Listen("tcp", ...).
func parseTCPListen(s string) (string, error) {
	if strings.HasPrefix(s, "tcp://") {
		s = strings.TrimPrefix(s, "tcp://")
	}
	if _, _, err := net.SplitHostPort(s); err != nil {
		return "", fmt.Errorf("invalid --listen %q (want tcp://host:port): %w", s, err)
	}
	return s, nil
}

// tcpAuthToken loads the bearer token from preferences. TCP mode without
// a configured token is refused — a TCP listener with no auth would be a
// trivial way to expose every Clank session to anyone on the network.
func tcpAuthToken() (string, error) {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return "", fmt.Errorf("load preferences: %w", err)
	}
	if prefs.RemoteHub == nil || strings.TrimSpace(prefs.RemoteHub.AuthToken) == "" {
		return "", fmt.Errorf("--listen tcp:// requires preferences.remote_hub.auth_token to be set")
	}
	return prefs.RemoteHub.AuthToken, nil
}

// buildSyncReceiver constructs a sync.Receiver for the cloud-hub mode,
// creating the mirror root under config.Dir()/sync. The hub's Store is
// reused for sync persistence — the cloud hub already owns it for
// session metadata, and migration v16 added the synced_repos /
// synced_branches tables to the same schema.
func buildSyncReceiver(s *hub.Service) (*clanksync.Receiver, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, fmt.Errorf("config dir: %w", err)
	}
	mirrors, err := clanksync.NewMirrorRoot(filepath.Join(dir, "sync"))
	if err != nil {
		return nil, fmt.Errorf("mirror root: %w", err)
	}
	return clanksync.NewReceiver(mirrors, s.Store, log.Default()), nil
}

// bearerAuthMiddleware enforces a static bearer token on all requests.
// Constant-time comparison so the token isn't trivially leaked through
// timing. A mismatch returns 401 without further detail.
func bearerAuthMiddleware(next http.Handler, token string) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		provided := []byte(auth[len(prefix):])
		if subtle.ConstantTimeCompare(provided, expected) != 1 {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
