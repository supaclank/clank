// clank-host is the Host plane binary. It owns the BackendManagers and
// SessionBackends and serves the host HTTP API on a Unix socket.
//
// In production it is spawned as a child process by clankd (the Hub).
// clankd connects via internal/host/client.NewUnixHTTP and routes every
// HUB-tagged operation through the wire.
//
// Usage:
//
//	clank-host --socket /path/to/host.sock
//
// On SIGINT/SIGTERM the server shuts down gracefully and host.Service
// stops every registered backend.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostmux "github.com/acksell/clank/internal/host/mux"
	"github.com/acksell/clank/internal/socketutil"
)

func main() {
	socket := flag.String("socket", "", "Path to Unix socket to listen on (required)")
	flag.Parse()

	if *socket == "" {
		fmt.Fprintln(os.Stderr, "clank-host: --socket is required")
		os.Exit(2)
	}

	if err := run(*socket); err != nil {
		log.Fatalf("clank-host: %v", err)
	}
}

func run(socket string) error {
	lg := log.New(os.Stderr, "[clank-host] ", log.LstdFlags)

	// Remove stale socket file from a prior crashed run. Refuses to
	// touch non-socket files so a bad --socket value cannot clobber
	// user data.
	if err := socketutil.RemoveStale(socket); err != nil {
		return err
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socket, err)
	}
	// Restrict the socket to the owner. Without an explicit chmod the
	// socket inherits the process umask and any local user could reach
	// the host control plane.
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod %s: %w", socket, err)
	}

	// Build host with both backend managers. Phase 1: hard-coded set
	// matching daemoncli's old population.
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode:   host.NewOpenCodeBackendManager(),
			agent.BackendClaudeCode: host.NewClaudeBackendManager(),
		},
		Log: lg,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.Init(ctx, func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		lg.Printf("warning: host.Init: %v", err)
	}

	srv := &http.Server{Handler: hostmux.New(svc, lg).Handler()}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	serveErr := make(chan error, 1)
	go func() {
		lg.Printf("listening on %s", socket)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case sig := <-sigCh:
		lg.Printf("received signal %v, shutting down", sig)
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
	}

	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		lg.Printf("http shutdown: %v", err)
	}
	svc.Shutdown()
	cancel()
	if err := socketutil.RemoveStale(socket); err != nil {
		lg.Printf("socket cleanup: %v", err)
	}
	lg.Println("stopped")
	return nil
}
