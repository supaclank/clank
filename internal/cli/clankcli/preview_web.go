package clankcli

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/webpreview"
)

// runWebPreview is the KindWeb arm of `clank preview`: no QR, no LAN.
// The overlay-injecting proxy (internal/webpreview) fronts the Vite dev
// server on one loopback origin that also serves the overlay assets and
// relays /__clank/api/* to the daemon — the browser twin of the phone
// flow's front door + native overlay.
//
// sessionID is non-empty when a prompt argument already started an
// agent; the overlay picks that session up instead of lazily creating
// one on first message.
func runWebPreview(sigCtx context.Context, projectDir, sockPath, sessionID, backend string, devPort, listenPort int) error {
	fmt.Println("Waiting for the dev server to come up…")
	if err := waitHTTPReady(sigCtx, devPort, 10*time.Minute); err != nil {
		return fmt.Errorf("dev server on port %d never came up (first-run installs can be slow; re-run to retry): %w", devPort, err)
	}

	token, err := randomToken(32)
	if err != nil {
		return fmt.Errorf("generate overlay token: %w", err)
	}
	engine := resolveVoiceEngine(sigCtx)
	if closer, ok := engine.(io.Closer); ok {
		defer closer.Close() // drop the warm model process with the preview
	}
	srv, err := webpreview.Start(webpreview.Options{
		UpstreamPort:     devPort,
		DaemonSocketPath: sockPath,
		Token:            token,
		Engine:           engine,
		ListenPort:       listenPort,
		OverlayConfig: map[string]any{
			"hostname":   host.HostLocal,
			"local_path": projectDir,
			"backend":    backend,
			"name":       filepath.Base(projectDir),
			"session_id": sessionID,
		},
	})
	if err != nil {
		return fmt.Errorf("start overlay proxy: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	printWebPreviewBanner(srv.URL, engine)
	_ = openBrowser(srv.URL) // best-effort; the banner printed the URL
	<-sigCtx.Done()
	fmt.Println("\nShutting down preview…")
	return nil
}

// resolveVoiceEngine picks the dictation engine for this preview:
//
//  1. CLANK_VOICE_ASR_CMD — explicit user override, always wins.
//  2. A clank-voice binary (sibling of clank, then PATH) — the
//     sherpa-onnx engine; its model set (~670 MB, one-time) is
//     downloaded here, before the banner, with terminal progress.
//  3. Neither → voice off; the banner says how to enable it.
//
// Errors degrade to voice-off with a warning rather than failing the
// preview — dictation is an accessory to the overlay, not a dependency.
func resolveVoiceEngine(ctx context.Context) webpreview.Engine {
	if e := webpreview.EngineFromEnv(); e != nil {
		return e
	}
	bin, err := webpreview.FindClankVoice()
	if err != nil {
		return nil
	}
	dir, err := webpreview.DefaultModelsDir()
	if err != nil {
		fmt.Println(styleWarn.Render("voice off: " + err.Error()))
		return nil
	}
	if !webpreview.ModelsPresent(dir) {
		fmt.Printf("Downloading the voice model (~670 MB, one-time) to %s…\n", dir)
		if derr := webpreview.EnsureModels(ctx, dir, printModelProgress); derr != nil {
			fmt.Println()
			fmt.Println(styleWarn.Render("voice off: model download failed: " + derr.Error()))
			return nil
		}
		fmt.Println()
	}
	engine := webpreview.NewSherpaEngine(bin, dir, log.Default())
	engine.Prewarm() // load the model now, not on the first key-hold
	return engine
}

// printModelProgress renders a single carriage-return progress line.
func printModelProgress(file string, index, count int, done, total int64) {
	if total > 0 {
		fmt.Printf("\r  [%d/%d] %-18s %3d%% (%d MB)", index, count, file, done*100/total, done>>20)
	} else {
		fmt.Printf("\r  [%d/%d] %-18s %d MB", index, count, file, done>>20)
	}
}

// waitHTTPReady polls the dev server's loopback port until it answers
// HTTP at all. Any response counts — a SvelteKit error page mid-compile
// still means Vite is up and the overlay can front it. The generous
// timeout wraps the same first-run `bun install` the daemon's own
// readiness probe allows for.
func waitHTTPReady(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out after %s", timeout)
			}
		}
	}
}

func printWebPreviewBanner(url string, engine webpreview.Engine) {
	fmt.Println()
	fmt.Printf("  Preview:  %s\n", styleCmdHint.Render(url))
	fmt.Println()
	fmt.Println("  Use the app normally — the clank overlay is one hotkey away:")
	fmt.Println("    ⇪ caps lock   summon / hide the prompt box (tap its header for chat)")
	fmt.Println("    hold ⌘ or ⌃   point at elements to attach them as context")
	if engine != nil {
		fmt.Println("    hold space    talk, with the composer empty " + styleDim.Render("("+engine.Describe()+")"))
	} else {
		fmt.Println(styleDim.Render("    voice off — install clank-voice (voice-engine/) or set " + webpreview.EngineEnvVar))
	}
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop the preview and shut everything down.")
}
