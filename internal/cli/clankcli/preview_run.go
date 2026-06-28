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

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
)

// runPreview boots an ephemeral LAN gateway for the project in projectDir,
// creates a session, starts its Metro dev server, and renders a QR the
// phone scans to pair + preview. Blocks until interrupted, then tears the
// whole stack down.
func runPreview(projectDir, prompt, backend string, port int) error {
	if strings.TrimSpace(prompt) == "" {
		// A session needs a prompt (or attachment) to start, and the
		// session is what materializes the worktree Metro runs on.
		// Prompt-free "just show my app" preview is a planned follow-up
		// (it needs a worktree-without-session primitive on the host).
		return fmt.Errorf("clank preview needs a prompt for now, e.g.\n  clank preview \"add a dark mode toggle\"\nthe agent starts working and you watch it live on your phone")
	}

	projectDir, err := resolveProjectDir(projectDir)
	if err != nil {
		return err
	}
	bt, err := resolveBackend(backend)
	if err != nil {
		return err
	}

	ip, err := lanIP()
	if err != nil {
		return err
	}
	// Make Metro advertise the LAN IP in its manifest so the phone can
	// fetch the bundle. The preview spawn inherits our process env, so
	// setting it here threads it all the way down to `expo start`.
	if err := os.Setenv("REACT_NATIVE_PACKAGER_HOSTNAME", ip.String()); err != nil {
		return fmt.Errorf("set packager hostname: %w", err)
	}

	dir, err := config.Dir()
	if err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	dataDir := filepath.Join(dir, "preview-host")

	fmt.Println("Starting preview gateway…")
	gw, err := startPreviewGateway(ip, port, dataDir, log.Default())
	if err != nil {
		return fmt.Errorf("start preview gateway: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		gw.Shutdown(ctx)
	}()

	client := daemonclient.NewTCPClient(gw.BaseURL, gw.Token)
	if err := waitGatewayReady(client); err != nil {
		return err
	}

	// Generous timeout: the first request spawns clank-host, and a cold
	// preview start runs `npm install` before Metro comes up.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Println("Creating session…")
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend:  bt,
		Hostname: host.HostLocal,
		GitRef:   agent.GitRef{LocalPath: projectDir},
		Prompt:   prompt,
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	wid := info.GitRef.WorktreeID
	if wid == "" {
		return fmt.Errorf("session %s created but no worktree id was returned", info.ID)
	}

	fmt.Println("Starting Metro dev server (first run installs dependencies — this can take a few minutes)…")
	status, err := client.Preview(wid).Start(ctx)
	if err != nil {
		return fmt.Errorf("start preview: %w", err)
	}
	if status.Port == 0 {
		return fmt.Errorf("preview started but Metro port is unknown (state=%s)", status.State)
	}
	// http (not exp://) — the phone fetches this as the Expo manifest base,
	// and the preview launcher keeps LAN http URLs on http. REACT_NATIVE_
	// PACKAGER_HOSTNAME (set above) makes Metro advertise this same LAN IP.
	previewURL := fmt.Sprintf("http://%s:%d", ip, status.Port)

	link := PreviewLink{
		GatewayURL: gw.BaseURL,
		Token:      gw.Token,
		SessionID:  info.ID,
		WorktreeID: wid,
		PreviewURL: previewURL,
		Name:       filepath.Base(projectDir),
	}
	linkStr, err := link.Encode()
	if err != nil {
		return err
	}

	// Best-effort Metro stop on exit; the gateway shutdown also reaps it.
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = client.Preview(wid).Stop(sctx)
	}()

	printPreviewBanner(linkStr, gw.BaseURL, previewURL)
	waitForInterrupt()
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

// waitGatewayReady polls /ping until the ephemeral gateway accepts, or
// times out. The gateway serves immediately, so this returns fast; it
// exists to fail clearly if the listener didn't come up.
func waitGatewayReady(client *daemonclient.Client) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := client.Ping(ctx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("preview gateway did not become ready: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitForInterrupt() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(ch)
	<-ch
}
