package clankcli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"
)

// runAttachedPreview fronts a user-owned HTTP server. It owns the overlay
// proxy and any daemon it starts, but never the upstream process.
func runAttachedPreview(projectDir string, upstreamURL *url.URL, backend string, listenPort int) error {
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, sockPath, startedDaemon, err := ensurePreviewDaemon()
	if err != nil {
		return err
	}
	if startedDaemon {
		defer func() {
			fmt.Println("Stopping the daemon clank preview started…")
			stopLocalDaemon()
		}()
	}

	// Same first-run question the launched-server path asks, and for the
	// same reason: the overlay's agent is the point, so settle it before
	// the proxy takes over the terminal.
	if err := offerPreviewAgentConnect(sigCtx, client, backend, os.Stdin, os.Stdout); err != nil {
		return err
	}

	bt, err := resolveBackend(backend, os.Stderr)
	if err != nil {
		return err
	}
	return runWebPreview(sigCtx, projectDir, sockPath, string(bt), upstreamURL, listenPort, "")
}
