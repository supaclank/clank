package clankcli

import (
	"context"
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
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
)

// runPreview makes the local daemon reachable from a phone on the LAN and
// serves the current folder's Expo app, Expo-style. Pairing, the dev
// server, and the agent session are independent: a prompt is optional and
// only starts an agent. Blocks until interrupted, then tears down — and
// stops the daemon only if it started it.
func runPreview(projectDir, prompt, backend string, port int) error {
	projectDir, err := resolveProjectDir(projectDir)
	if err != nil {
		return err
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ip, err := lanIP()
	if err != nil {
		return err
	}
	// Make Metro advertise the LAN IP in its manifest so the phone can
	// fetch the bundle. The dev server inherits our process env, so
	// setting it here threads down to `expo start`.
	if err := os.Setenv("REACT_NATIVE_PACKAGER_HOSTNAME", ip.String()); err != nil {
		return fmt.Errorf("set packager hostname: %w", err)
	}

	// Reuse the running daemon, or start one — and remember which, so we
	// only stop it on exit if we started it.
	wasRunning, _, _ := daemonclient.IsRunning()
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

	// Per-run key for the in-place dev server. The server runs against
	// projectDir itself; the key just lets us stop it on exit.
	previewKey := ulid.Make().String()

	// Generous timeout: a cold preview start runs `npm install` first.
	// Derives from sigCtx so Ctrl+C during startup aborts the wait.
	startCtx, cancel := context.WithTimeout(sigCtx, 10*time.Minute)
	defer cancel()

	fmt.Println("Starting the dev server on this folder (first run installs dependencies)…")
	status, err := client.Preview(previewKey).Start(startCtx, projectDir)
	if err != nil {
		return fmt.Errorf("start preview (is this an Expo project?): %w", err)
	}
	if status.Port == 0 {
		return fmt.Errorf("preview started but the dev server port is unknown (state=%s)", status.State)
	}
	// http, not exp:// — the phone fetches this as the Expo manifest base,
	// and the preview launcher keeps LAN http URLs on http.
	previewURL := fmt.Sprintf("http://%s:%d", ip, status.Port)
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = client.Preview(previewKey).Stop(sctx)
	}()

	// A prompt is optional. If you pass one, kick the agent off now and
	// watch it on your phone. If not, no session is created here — the
	// phone creates one (this folder as the GitRef, the first message as
	// the prompt) when you start talking in the preview overlay.
	bt, err := resolveBackend(backend)
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

func resolveProjectDir(projectDir string) (string, error) {
	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		projectDir = cwd
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolve project dir %q: %w", projectDir, err)
	}
	return abs, nil
}

func resolveBackend(backend string) (agent.BackendType, error) {
	if backend != "" {
		return agent.ParseBackend(backend)
	}
	prefs, _ := config.LoadPreferences()
	resolved, err := agent.ResolveBackendPreference(prefs.DefaultBackend)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; using %s\n", err, resolved)
	}
	return resolved, nil
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
