package clankcli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/url"
	"path/filepath"
	"time"

	"github.com/supaclank/clank/internal/config"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/webpreview"
)

// webPreviewParams carries everything runWebPreview needs to front an
// upstream. SessionID, when set, seeds the overlay with an existing
// agent session (`--attach`) instead of creating one on first prompt.
type webPreviewParams struct {
	ProjectDir  string
	SockPath    string
	Backend     string
	UpstreamURL *url.URL
	ListenPort  int
	DisplayName string
	SessionID   string
}

// runWebPreview is the browser arm of `clank preview`: no QR, no LAN.
// The overlay-injecting proxy (internal/webpreview) fronts an upstream
// on one loopback origin that also serves the overlay assets and relays
// /__clank/api/* to the daemon — the browser twin of the phone flow's
// front door + native overlay. The upstream may be a daemon-managed dev
// server or an explicitly attached local origin.
//
// Daemon-managed callers wait for readiness first; explicit attach mode leaves
// upstream readiness to its owner.
func runWebPreview(sigCtx context.Context, p webPreviewParams) error {
	token, err := randomToken(32)
	if err != nil {
		return fmt.Errorf("generate overlay token: %w", err)
	}
	engine := resolveVoiceEngine(sigCtx)
	if closer, ok := engine.(io.Closer); ok {
		defer closer.Close() // drop the warm model process with the preview
	}
	if sigCtx.Err() != nil {
		// The model download can run for minutes; a Ctrl+C during it must
		// not still spin up the proxy server and open a browser tab.
		return sigCtx.Err()
	}
	overlayConfig := map[string]any{
		"hostname":   host.HostLocal,
		"local_path": p.ProjectDir,
		"backend":    p.Backend,
		"name":       previewOverlayName(p.ProjectDir, p.DisplayName),
	}
	if p.SessionID != "" {
		overlayConfig["session_id"] = p.SessionID
	}
	srv, err := webpreview.Start(webpreview.Options{
		UpstreamURL:            p.UpstreamURL,
		DaemonSocketPath:       p.SockPath,
		Token:                  token,
		Engine:                 engine,
		DictationEngine:        loadDictationPreference(),
		PersistDictationEngine: persistDictationPreference,
		LauncherSeen:           loadLauncherSeenPreference(),
		PersistLauncherSeen:    persistLauncherSeenPreference,
		ListenPort:             p.ListenPort,
		OverlayConfig:          overlayConfig,
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

func previewOverlayName(projectDir, displayName string) string {
	if displayName != "" {
		return displayName
	}
	return filepath.Base(projectDir)
}

// loadDictationPreference reads the overlay's persisted local-vs-
// webspeech choice. Unreadable prefs or an unknown stored value degrade
// to "unchosen" with a warning (the overlay asks again) — dictation
// must never block the preview.
func loadDictationPreference() webpreview.DictationEngine {
	prefs, err := config.LoadPreferences()
	if err != nil {
		fmt.Println(styleWarn.Render("dictation choice reset (couldn't read preferences): " + err.Error()))
		return ""
	}
	if prefs.WebPreviewDictation == "" {
		return ""
	}
	dictation, ok := webpreview.ParseDictationEngine(prefs.WebPreviewDictation)
	if !ok {
		fmt.Println(styleWarn.Render(fmt.Sprintf("dictation choice reset (unknown web_preview_dictation %q in preferences)", prefs.WebPreviewDictation)))
		return ""
	}
	return dictation
}

// persistDictationPreference stores the overlay picker's choice for
// future preview runs.
func persistDictationPreference(engine webpreview.DictationEngine) error {
	return config.UpdatePreferences(func(p *config.Preferences) {
		p.WebPreviewDictation = string(engine)
	})
}

func loadLauncherSeenPreference() bool {
	prefs, err := config.LoadPreferences()
	if err != nil {
		fmt.Println(styleWarn.Render("launcher introduction reset (couldn't read preferences): " + err.Error()))
		return false
	}
	return prefs.WebPreviewLauncherSeen
}

func persistLauncherSeenPreference() error {
	return config.UpdatePreferences(func(p *config.Preferences) {
		p.WebPreviewLauncherSeen = true
	})
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

func printWebPreviewBanner(url string, engine webpreview.Engine) {
	fmt.Println()
	fmt.Printf("  Preview:  %s\n", styleCmdHint.Render(url))
	fmt.Println()
	fmt.Println("  Use the app normally — the clank overlay is one hotkey away:")
	fmt.Println("    ⌘E / Ctrl+E   summon / hide the prompt box (tap its header for chat)")
	fmt.Println("    hold ⌘ or ⌃   point at elements to attach them as context")
	if engine != nil {
		fmt.Println("    ⇪ caps lock   tap to talk, tap again to transcribe " + styleDim.Render("(local: "+engine.Describe()+")"))
	} else {
		fmt.Println("    ⇪ caps lock   tap to talk, tap again to transcribe " + styleDim.Render("(browser Web Speech API, where supported — audio goes to the browser vendor)"))
		fmt.Println(styleDim.Render("                  install clank-voice (voice-engine/) or set " + webpreview.EngineEnvVar + " for fully-local dictation"))
	}
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop the preview and shut everything down.")
}

// randomToken mints a URL-safe random bearer (web preview overlay auth).
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
