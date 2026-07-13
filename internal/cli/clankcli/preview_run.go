package clankcli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/preview"
)

// runPreview serves the current folder's app for live preview and tears
// everything down on interrupt (stopping the daemon only if it started
// it). What "preview" means depends on what Detect finds:
//
//   - Expo: expose the daemon to the phone over the LAN behind a
//     pairing token and print the QR (the original flow).
//   - Vite web: front the dev server with the overlay-injecting proxy
//     and open it in the browser (runWebPreview).
//
// Pairing/proxy, the dev server, and the agent session stay independent:
// a prompt argument is optional and only pre-starts an agent.
func runPreview(projectDir, prompt, backend string, port int) error {
	projectDir, err := resolveProjectDir(projectDir)
	if err != nil {
		return err
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// LAN details are only needed for the phone path, but Metro reads
	// REACT_NATIVE_PACKAGER_HOSTNAME from the daemon's environment and
	// the daemon inherits ours only when we're the one starting it — so
	// this must run before ensureDaemon, before the Kind is knowable.
	// Web previews are loopback-only and need neither (a laptop with no
	// LAN IP can still web-preview, hence the deferred error).
	ip, ipErr := lanIP()
	if ipErr == nil {
		// Make Metro advertise the LAN IP in its manifest so the phone can
		// fetch the bundle. The dev server inherits our process env, so
		// setting it here threads down to `expo start`.
		if err := os.Setenv("REACT_NATIVE_PACKAGER_HOSTNAME", ip.String()); err != nil {
			return fmt.Errorf("set packager hostname: %w", err)
		}
	}

	// Reuse the running daemon, or start one — and remember which, so we
	// only stop it on exit if we started it.
	wasRunning, _, isRunningErr := daemonclient.IsRunning()
	if isRunningErr != nil {
		wasRunning = true // safe side: avoid stopping an unrelated daemon if IsRunning fails
	}
	client, err := ensureDaemon()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	if !wasRunning {
		defer func() {
			fmt.Println("Stopping the daemon clank preview started…")
			stopLocalDaemon()
		}()
	}

	sockPath, err := daemonclient.SocketPath()
	if err != nil {
		return fmt.Errorf("daemon socket path: %w", err)
	}

	// Per-run key for the in-place dev server. The server runs against
	// projectDir itself; the key just lets us stop it on exit.
	previewKey := ulid.Make().String()

	// Generous timeout: a cold preview start runs `bun install` first.
	// Derives from sigCtx so Ctrl+C during startup aborts the wait.
	startCtx, cancel := context.WithTimeout(sigCtx, 10*time.Minute)
	defer cancel()

	fmt.Println("Starting the dev server on this folder (first run installs dependencies)…")
	status, err := client.Preview(previewKey).Start(startCtx, projectDir)
	if err != nil {
		// The Expo/Vite hint is only true for the daemon's "no app
		// detected here" answer; any other failure (path resolution,
		// spawn error) surfaces verbatim so it isn't mislabeled.
		if errors.Is(err, daemonclient.ErrNotPreviewable) {
			return fmt.Errorf("start preview (is this an Expo or Vite project?): %w", err)
		}
		return fmt.Errorf("start preview: %w", err)
	}
	if status.Port == 0 {
		return fmt.Errorf("preview started but the dev server port is unknown (state=%s)", status.State)
	}
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = client.Preview(previewKey).Stop(sctx)
	}()

	// A prompt is optional. If you pass one, kick the agent off now and
	// watch it work in the preview. If not, no session is created here —
	// the overlay (phone or browser) creates one (this folder as the
	// GitRef, the first message as the prompt) when you start talking.
	bt, err := resolveBackend(backend, os.Stderr)
	if err != nil {
		return err
	}
	var sessionID string
	if strings.TrimSpace(prompt) != "" {
		fmt.Println("Starting the agent on this folder…")
		info, cerr := client.Sessions().Create(startCtx, agent.StartRequest{
			Backend:  bt,
			Hostname: host.HostLocal,
			GitRef:   agent.GitRef{LocalPath: projectDir},
			Prompt:   prompt,
		})
		if cerr != nil {
			return fmt.Errorf("create session: %w", cerr)
		}
		sessionID = info.ID
	}

	if status.Kind == string(preview.KindWeb) {
		return runWebPreview(sigCtx, projectDir, sockPath, sessionID, string(bt), status.Port, port)
	}

	// Phone (Expo) path from here down.
	if ipErr != nil {
		return ipErr
	}
	fmt.Println("Opening this folder to your phone…")
	fd, err := startPreviewFrontDoor(ip, port, sockPath, log.Default())
	if err != nil {
		return fmt.Errorf("start preview front door: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fd.Shutdown(ctx)
	}()

	// http, not exp:// — the phone fetches this as the Expo manifest base,
	// and the preview launcher keeps LAN http URLs on http.
	previewURL := fmt.Sprintf("http://%s:%d", ip, status.Port)

	link := PreviewLink{
		GatewayURL: fd.BaseURL,
		Token:      fd.Token,
		PreviewURL: previewURL,
		SessionID:  sessionID, // empty unless a prompt was passed
		LocalPath:  projectDir,
		Backend:    string(bt),
		Name:       filepath.Base(projectDir),
	}
	linkStr, err := link.Encode()
	if err != nil {
		return err
	}

	printPreviewBanner(linkStr, fd.BaseURL, previewURL)
	<-sigCtx.Done()
	fmt.Println("\nShutting down preview…")
	return nil
}

// stopLocalDaemon sends SIGINT to the running daemon (graceful shutdown).
// Best-effort: used only when `clank preview` was the one that started it.
func stopLocalDaemon() {
	running, pid, err := daemonclient.IsRunning()
	if err != nil || !running {
		return
	}
	if proc, perr := os.FindProcess(pid); perr == nil {
		_ = proc.Signal(os.Interrupt)
	}
}
