package daemoncli

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/gateway"
	"github.com/acksell/clank/internal/provisioner"
	"github.com/acksell/clank/internal/socketutil"
)

// openHubListener creates the listener for the configured mode and a
// cleanup func that removes on-disk artifacts.
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
	sockPath, err := daemonclient.SocketPath()
	if err != nil {
		return nil, nil, fmt.Errorf("socket path: %w", err)
	}
	// Probe before unlink so we don't yank an active peer's listener.
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

// tcpAuthToken returns the configured bearer or refuses startup —
// an unauthenticated TCP listener would expose every session to the
// network.
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

// runGatewayServer mounts the gateway on opts.Listen.
//
// Modes:
//   - Unix socket (default): file mode is the gate.
//   - TCP (opts.Listen non-empty): wraps with bearer-token middleware,
//     enforced before reaching the gateway's own auth.
//
// Both modes write the PID file at daemonclient.PIDPath().
func runGatewayServer(prov provisioner.Provisioner, opts ServerOptions) error {
	pidPath, err := daemonclient.PIDPath()
	if err != nil {
		return fmt.Errorf("pid path: %w", err)
	}

	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: prov,
		Auth:        gateway.PermissiveAuth{},
		ResolveUserID: func(*http.Request) string {
			return "local" // single-user laptop today
		},
	}, log.Default())
	if err != nil {
		return fmt.Errorf("build gateway: %w", err)
	}

	handler := gw.Handler()
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

	srv := &http.Server{Handler: handler}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("gateway listening on %s", listener.Addr())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case sig := <-sigCh:
		log.Printf("received signal %v, shutting down gateway", sig)
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("gateway serve: %w", err)
		}
	}

	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("gateway shutdown: %v", err)
	}
	return nil
}

// bearerAuthMiddleware enforces a static bearer with constant-time
// comparison. A mismatch returns 401 without further detail.
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
