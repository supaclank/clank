package clankcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/host/preview"
	"github.com/supaclank/clank/internal/lannet"
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
// it). What "preview" means depends on the resolved launch:
//
//   - Expo: expose the daemon to the phone over the LAN behind a
//     pairing token and print the QR (the original flow).
//   - Configured web: front the dev server with the overlay-injecting
//     proxy and open it in the browser (runWebPreview).
func runPreview(projectDir, launchName, backend string, port int) error {
	return runPreviewWithDisplayName(projectDir, launchName, backend, port, "")
}

func runPreviewWithDisplayName(projectDir, launchName, backend string, port int, displayName string) error {
	isProjectExplicit := projectDir != ""
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

	// Ask about connecting an agent BEFORE anything long-running starts:
	// once the dev server is up we're printing install/bundler output for
	// minutes, and a picker seizing the terminal after that is hostile.
	// Runs after ensurePreviewDaemon because the catalog read needs the
	// daemon, and before resolveBackend so a just-connected backend is the
	// one this preview resolves.
	if err := offerPreviewAgentConnect(sigCtx, client, backend, os.Stdin, os.Stdout); err != nil {
		return err
	}

	bt, err := resolveBackend(backend, os.Stderr)
	if err != nil {
		return err
	}

	// The preview key is the folder's slug — the identity the daemon
	// resolves back to projectDir (host.previewWorkDirFor), the phone
	// polls status/logs with (via the QR), and a re-run reattaches
	// through: same folder, same key, same running server.
	previewKey := host.LocalRepoSlug(projectDir)

	// Derives from sigCtx so Ctrl+C during startup aborts the wait.
	startCtx, cancel := context.WithTimeout(sigCtx, previewStartupTimeout)
	defer cancel()

	pv := client.Preview(previewKey)
	if launchName != "" {
		pv = pv.Named(launchName)
	}
	var setupResult *previewSetupResult
	fmt.Println("Starting the preview dev server…")
	status, err := pv.Start(startCtx)
	if err != nil {
		if errors.Is(err, daemonclient.ErrPreviewSetupRequired) {
			setupResult, err = runPreviewSetup(startCtx, client, bt, projectDir, os.Stdin, os.Stdout)
			if err != nil {
				return err
			}
			startName, substitutionNotice := previewStartNameAfterSetup(launchName, setupResult.Launch.Name)
			if substitutionNotice != "" {
				fmt.Println(substitutionNotice)
			}
			pv = client.Preview(previewKey).Named(startName)
			fmt.Println("Starting the preview dev server with the generated configuration…")
			status, err = pv.Start(startCtx)
			if err != nil {
				return previewSetupSessionError(setupResult.ProjectRoot, setupResult.SessionID, fmt.Errorf("start preview after one-time setup: %w", err))
			}
		} else {
			return fmt.Errorf("start preview: %w", err)
		}
	}
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = pv.Stop(sctx)
	}()
	if status.ServiceName == "" {
		err := fmt.Errorf("preview started without a service name")
		if setupResult != nil {
			return previewSetupSessionError(setupResult.ProjectRoot, setupResult.SessionID, err)
		}
		return err
	}
	// Follow-up web operations target the resolved config entry. Expo keeps
	// its historical unnamed selection because its service is also called
	// "default", which may be an explicit web entry in an Expo repository.
	if status.Kind == string(preview.KindWeb) {
		pv = client.Preview(previewKey).Named(status.ServiceName)
	}
	if status.Kind == string(preview.KindWeb) {
		projectDir, err = managedPreviewProjectDir(projectDir, isProjectExplicit)
		if err != nil {
			return fmt.Errorf("resolve managed preview project context: %w", err)
		}
	}
	if setupResult != nil && status.Kind != string(preview.KindWeb) {
		return previewSetupSessionError(setupResult.ProjectRoot, setupResult.SessionID, fmt.Errorf("generated launch resolved to unexpected preview kind %q", status.Kind))
	}
	if status.Port == 0 {
		err := fmt.Errorf("preview started but the dev server port is unknown (state=%s)", status.State)
		if setupResult != nil {
			return previewSetupSessionError(setupResult.ProjectRoot, setupResult.SessionID, err)
		}
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
					_, _ = pv.Status(tctx)
					tcancel()
				}
			}
		}()
		fmt.Println("Waiting for the dev server to come up…")
		status, err = waitPreviewReady(sigCtx, pv, status, previewStartupTimeout)
		if err != nil {
			if setupResult != nil {
				return previewSetupSessionError(setupResult.ProjectRoot, setupResult.SessionID, err)
			}
			return err
		}
		if setupResult != nil {
			completePreviewSetupSession(client, setupResult.SessionID, os.Stdout)
			fmt.Println("One-time preview setup complete.")
		}
		upstreamURL := previewLoopbackURL(status.Port)
		return runWebPreview(sigCtx, projectDir, sockPath, string(bt), upstreamURL, port, displayName)
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
	if err := watchExpoPreview(sigCtx, pv, status.State); err != nil {
		return err
	}
	fmt.Println("\nShutting down preview…")
	return nil
}

// previewStartNameAfterSetup returns the preview entry to start once
// one-time setup has run. Setup only guarantees its generated default
// entry exists, so a requested name from before setup ran may not be
// defined — startName always falls back to the generated default, and
// notice is non-empty when that silently overrides the request.
func previewStartNameAfterSetup(requested, generatedDefault string) (startName, notice string) {
	if requested != "" && requested != generatedDefault {
		notice = fmt.Sprintf("Generated configuration named this preview %q; starting it instead of %q.", generatedDefault, requested)
	}
	return generatedDefault, notice
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
