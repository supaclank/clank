package clankcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/clankyaml"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/preview"
	"github.com/acksell/clank/internal/lannet"
)

// printPackagerNote tells the user which package manager the preview
// will install with when it isn't bun — the project's own lockfile or
// packageManager field decided (preview.ResolvePackager). Purely
// informational: detection failures stay silent here and surface
// through Start's canonical error path instead. No switching
// instructions: adopting bun (a bun.lock appearing) or a clank.yaml
// preview.install both flip detection on their own, and the docs
// cover the rest.
func printPackagerNote(projectDir string) {
	spec, err := preview.Detect(projectDir)
	if err != nil || spec == nil {
		return
	}
	switch preview.Packager(spec.Installer) {
	case preview.PackagerNPM, preview.PackagerPNPM, preview.PackagerYarn:
		fmt.Printf("Installing with %s (%s).\n", spec.Installer, spec.ToolEvidence)
		fmt.Println("Tip: bun is ~10x faster on cold worktrees and uses ~60x less disk.")
	}
}

// previewKeepaliveInterval paces the CLI's liveness pings against the
// daemon's idle reaper (preview.DefaultIdleTimeout, 15m) — wide margin
// so a couple of missed ticks never let a live preview get reaped.
const previewKeepaliveInterval = 1 * time.Minute

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

	// The preview key is the folder's slug — the identity the daemon
	// resolves back to projectDir (host.previewWorkDirFor), the phone
	// polls status/logs with (via the QR), and a re-run reattaches
	// through: same folder, same key, same running server.
	previewKey := host.LocalRepoSlug(projectDir)

	// Generous timeout: a cold preview start installs dependencies
	// first. Derives from sigCtx so Ctrl+C during startup aborts the wait.
	startCtx, cancel := context.WithTimeout(sigCtx, 10*time.Minute)
	defer cancel()

	printPackagerNote(projectDir)
	fmt.Println("Starting the dev server on this folder (first run installs dependencies)…")
	status, err := client.Preview(previewKey).Start(startCtx)
	if err != nil {
		// The framework hint is only true for the daemon's "no app
		// detected here" answer; any other failure (path resolution,
		// spawn error) surfaces verbatim so it isn't mislabeled.
		if errors.Is(err, daemonclient.ErrNotPreviewable) {
			return fmt.Errorf("start preview (no Expo, Next.js, or Vite app detected here — any other stack can declare its dev server via preview.command in %s): %w", clankyaml.FileName, err)
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
		sessionID, err = startPreviewAgent(startCtx, client, bt, projectDir, prompt)
		if err != nil {
			return err
		}
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
		return runWebPreview(sigCtx, projectDir, sockPath, sessionID, string(bt), status.Port, port)
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
		SessionID:  sessionID, // empty unless a prompt was passed
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

// startPreviewAgent creates the prompt-argument session for `clank
// preview <prompt>`: this folder as the GitRef, config from the
// backend's Default preset (creates without it are rejected by the
// host as config_incomplete).
func startPreviewAgent(ctx context.Context, client *daemonclient.Client, bt agent.BackendType, projectDir, prompt string) (string, error) {
	cfg, err := defaultPresetConfig(ctx, client, bt, host.HostLocal)
	if err != nil {
		return "", err
	}
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend:  bt,
		Hostname: host.HostLocal,
		GitRef:   agent.GitRef{LocalPath: projectDir},
		Prompt:   prompt,
		Config:   cfg,
	})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return info.ID, nil
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
