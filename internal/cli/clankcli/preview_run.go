package clankcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/preview"
	"github.com/acksell/clank/internal/lannet"
)

// previewKeepaliveInterval paces the CLI's liveness pings against the
// daemon's idle reaper (preview.DefaultIdleTimeout, 15m) — wide margin
// so a couple of missed ticks never let a live preview get reaped.
const (
	previewKeepaliveInterval = 1 * time.Minute
	previewStartupTimeout    = 10 * time.Minute
)

// runPreview serves the current folder's app for live preview and tears
// everything down on interrupt (stopping the daemon only if it started
// it). What "preview" means depends on what Detect finds:
//
//   - Expo: expose the daemon to the phone over the LAN behind a
//     pairing token and print the QR (the original flow).
//   - Vite web: front the dev server with the overlay-injecting proxy
//     and open it in the browser (runWebPreview).
func runPreview(projectDir, backend string, port int) error {
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
	ip, ipErr := lannet.LANIP()
	if ipErr != nil {
		// No physical LAN (ethernet unplugged, hotspot off) — a
		// tailnet-only laptop still serves Metro over WireGuard.
		if tn := lannet.TailnetIP(); tn != nil {
			ip, ipErr = tn, nil
		}
	}
	if ipErr == nil {
		// Make Metro advertise the LAN IP in its manifest so the phone can
		// fetch the bundle. The dev server inherits our process env, so
		// setting it here threads down to `expo start`.
		if err := os.Setenv("REACT_NATIVE_PACKAGER_HOSTNAME", ip.String()); err != nil {
			return fmt.Errorf("set packager hostname: %w", err)
		}
	}

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

	// The preview key is the folder's slug — the identity the daemon
	// resolves back to projectDir (host.previewWorkDirFor), the phone
	// polls status/logs with (via the QR), and a re-run reattaches
	// through: same folder, same key, same running server.
	previewKey := host.LocalRepoSlug(projectDir)

	// Generous timeout: a cold preview start runs `bun install` first.
	// Derives from sigCtx so Ctrl+C during startup aborts the wait.
	startCtx, cancel := context.WithTimeout(sigCtx, previewStartupTimeout)
	defer cancel()

	fmt.Println("Starting the dev server on this folder (first run installs dependencies)…")
	status, err := client.Preview(previewKey).Start(startCtx)
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

	bt, err := resolveBackend(backend, os.Stderr)
	if err != nil {
		return err
	}

	if status.Kind == string(preview.KindWeb) {
		// Keepalive: the daemon's idle reaper counts Status reads as
		// liveness, and nothing else does — the overlay proxy below runs
		// in this process, so no preview traffic ever crosses the daemon.
		// Status never spawns, so no shutdown ordering vs the Stop defer.
		go func() {
			ticker := time.NewTicker(previewKeepaliveInterval)
			defer ticker.Stop()
			for {
				select {
				case <-sigCtx.Done():
					return
				case <-ticker.C:
					tctx, tcancel := context.WithTimeout(sigCtx, 10*time.Second)
					_, _ = client.Preview(previewKey).Status(tctx)
					tcancel()
				}
			}
		}()
		fmt.Println("Waiting for the dev server to come up…")
		upstreamURL := previewLoopbackURL(status.Port)
		if err := waitHTTPReady(sigCtx, upstreamURL, previewStartupTimeout); err != nil {
			return fmt.Errorf("dev server on port %d never came up (first-run installs can be slow; re-run to retry): %w", status.Port, err)
		}
		return runWebPreview(sigCtx, projectDir, sockPath, string(bt), upstreamURL, port)
	}

	// Phone (Expo) path from here down. The daemon's bridge is the
	// phone's gateway — standing listeners, standing secret; the
	// trust-this-LAN prompt runs inside ensurePhoneReachable when a
	// plain LAN is the only path and hasn't been consented to.
	if ipErr != nil {
		return ipErr
	}
	bst, err := ensurePhoneReachable(sigCtx, client, os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Println("Opening this folder to your phone…")

	// http, not exp:// — the phone fetches this as the Expo manifest base,
	// and the preview launcher keeps LAN http URLs on http.
	previewURL := fmt.Sprintf("http://%s:%d", ip, status.Port)

	// The QR is all public: addresses + the laptop's identity key. A
	// new phone pairs via the typed-code ceremony (pairingLoop below);
	// a phone that already paired reconnects on its own.
	link := PreviewLink{
		GatewayURL: bst.URLs[0],
		Alts:       bst.URLs[1:],
		HostKey:    bst.HostKey,
		PreviewURL: previewURL,
		LocalPath:  projectDir,
		Backend:    string(bt),
		// Name is the laptop's gateway-picker label — its hostname, the
		// same as `clank pair`. NOT the project folder: the QR pairs the
		// phone to the LAPTOP, and the project rides LocalPath/WorktreeID.
		Name:       shortHostname(),
		WorktreeID: previewKey,
	}
	linkStr, err := link.Encode()
	if err != nil {
		return err
	}

	printPreviewBanner(linkStr, bst.URLs[0], previewURL)
	// Service inbound pairing ceremonies for the whole preview session:
	// a new phone that scans shows a code the terminal prompts you to
	// type. Returning phones just reconnect (probe path, no ceremony).
	go pairingLoop(sigCtx, client, os.Stdin, os.Stdout)
	if err := watchExpoPreview(sigCtx, client.Preview(previewKey), status.State); err != nil {
		return err
	}
	fmt.Println("\nShutting down preview…")
	return nil
}

// ensurePreviewDaemon reuses the running daemon or starts one, and
// reports whether this process started it (callers stop only a daemon
// they started — never an unrelated one).
func ensurePreviewDaemon() (client *daemonclient.Client, sockPath string, startedByUs bool, err error) {
	wasRunning, _, isRunningErr := daemonclient.IsRunning()
	if isRunningErr != nil {
		wasRunning = true // safe side: avoid stopping an unrelated daemon if IsRunning fails
	}
	client, err = ensureDaemon()
	if err != nil {
		return nil, "", false, fmt.Errorf("daemon: %w", err)
	}
	sockPath, err = daemonclient.SocketPath()
	if err != nil {
		return nil, "", false, fmt.Errorf("daemon socket path: %w", err)
	}
	return client, sockPath, !wasRunning, nil
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
