// clank-host is the Host plane binary. It owns the BackendManagers and
// SessionBackends and serves the host HTTP API on either a Unix socket
// (default for laptop hubs) or a TCP listener (for cloud sandboxes /
// in-process LocalLauncher tests).
//
// In production it is spawned as a child process by clankd (the Hub).
// clankd connects via internal/host/client and routes every HUB-tagged
// operation through the wire.
//
// Usage:
//
//	clank-host --socket /path/to/host.sock
//	clank-host --listen tcp://127.0.0.1:0    # auto-pick port
//
// On startup prints "listening on <addr>" — parents that picked port 0
// read this to discover the bound address.
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
	"strings"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostmux "github.com/acksell/clank/internal/host/mux"
	"github.com/acksell/clank/internal/socketutil"
)

func main() {
	socket := flag.String("socket", "", "Path to Unix socket to listen on (mutually exclusive with --listen)")
	listen := flag.String("listen", "", "Listener address: tcp://host:port (use :0 for auto-pick) or unix:///path. Mutually exclusive with --socket.")
	gitSyncSource := flag.String("git-sync-source", "", "Cloud-hub base URL to clone from instead of GitHub (e.g. http://hub.internal:7878). Empty = clone from RemoteURL directly. Used by sandboxes.")
	gitSyncToken := flag.String("git-sync-token", "", "Bearer token paired with --git-sync-source. Injected as Authorization header on clone.")
	flag.Parse()

	if *socket == "" && *listen == "" {
		fmt.Fprintln(os.Stderr, "clank-host: --socket or --listen is required")
		os.Exit(2)
	}
	if *socket != "" && *listen != "" {
		fmt.Fprintln(os.Stderr, "clank-host: --socket and --listen are mutually exclusive")
		os.Exit(2)
	}

	addr := *listen
	if addr == "" {
		addr = "unix://" + *socket
	}
	if err := run(addr, *gitSyncSource, *gitSyncToken); err != nil {
		log.Fatalf("clank-host: %v", err)
	}
}

// run binds the listener for addr (a "tcp://host:port" or "unix:///path"
// URL) and serves the host API on it until SIGINT/SIGTERM.
func run(addr, gitSyncSource, gitSyncToken string) error {
	lg := log.New(os.Stderr, "[clank-host] ", log.LstdFlags)

	ln, kind, sockPath, err := openListener(addr)
	if err != nil {
		return err
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode:   host.NewOpenCodeBackendManager(),
			agent.BackendClaudeCode: host.NewClaudeBackendManager(),
		},
		Log:           lg,
		GitSyncSource: gitSyncSource,
		GitSyncToken:  gitSyncToken,
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
		// Print the actual bound address so parents that asked for
		// port :0 (LocalLauncher) can read it from stderr.
		lg.Printf("listening on %s://%s", kind, ln.Addr().String())
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
	if sockPath != "" {
		if err := socketutil.RemoveStale(sockPath); err != nil {
			lg.Printf("socket cleanup: %v", err)
		}
	}
	lg.Println("stopped")
	return nil
}

// openListener parses addr and binds the appropriate listener. Returns
// the listener, the scheme ("tcp" or "unix"), and the socket path (for
// unix mode; empty for tcp).
func openListener(addr string) (net.Listener, string, string, error) {
	switch {
	case strings.HasPrefix(addr, "tcp://"):
		host := strings.TrimPrefix(addr, "tcp://")
		ln, err := net.Listen("tcp", host)
		if err != nil {
			return nil, "", "", fmt.Errorf("listen tcp %s: %w", host, err)
		}
		return ln, "tcp", "", nil

	case strings.HasPrefix(addr, "unix://"):
		path := strings.TrimPrefix(addr, "unix://")
		// Remove stale socket file from a prior crashed run. Refuses to
		// touch non-socket files so a bad path cannot clobber user data.
		if err := socketutil.RemoveStale(path); err != nil {
			return nil, "", "", err
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, "", "", fmt.Errorf("listen %s: %w", path, err)
		}
		// Restrict the socket to the owner.
		if err := os.Chmod(path, 0o600); err != nil {
			_ = ln.Close()
			return nil, "", "", fmt.Errorf("chmod %s: %w", path, err)
		}
		return ln, "unix", path, nil

	default:
		return nil, "", "", fmt.Errorf("unsupported listen address %q (want tcp:// or unix://)", addr)
	}
}
