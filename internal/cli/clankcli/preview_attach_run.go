package clankcli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"
)

// runAttachedPreview fronts a user-owned HTTP server. It owns the overlay
// proxy and any daemon it starts, but never the upstream process.
func runAttachedPreview(projectDir string, upstreamURL *url.URL, backend string, listenPort int, attachSession string) error {
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

	// Same interactive questions the launched-server path asks, and for
	// the same reason: the overlay's agent is the point, so settle them
	// before the proxy takes over the terminal.
	attachedSession, err := resolveAttachSession(sigCtx, client, attachSession, projectDir, os.Stdin, os.Stdout)
	if errors.Is(err, errPreviewAttachAborted) {
		fmt.Println("Preview canceled.")
		return nil
	}
	if err != nil {
		return err
	}
	if attachedSession != nil {
		bt, err := attachedSessionBackend(attachedSession, backend)
		if err != nil {
			return err
		}
		backend = string(bt)
	}
	if err := offerPreviewAgentConnect(sigCtx, client, backend, os.Stdin, os.Stdout); err != nil {
		return err
	}

	bt, err := resolveBackend(backend, os.Stderr)
	if err != nil {
		return err
	}
	return runWebPreview(sigCtx, webPreviewParams{
		ProjectDir:  projectDir,
		SockPath:    sockPath,
		Backend:     string(bt),
		UpstreamURL: upstreamURL,
		ListenPort:  listenPort,
		SessionID:   attachedSessionID(attachedSession),
	})
}
