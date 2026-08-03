package clankcli

import (
	"context"
	"fmt"
	"time"

	"github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host/preview"
)

// previewStartPollInterval paces status polls while the dev server is
// still starting — fast enough that the "ready" line lands promptly
// after a minutes-long first-run install.
const previewStartPollInterval = 1 * time.Second

// watchExpoPreview blocks until ctx cancels (Ctrl+C) or the dev server
// settles failed/stopped. It doubles as the idle-reaper keepalive:
// every poll is a Status read the daemon counts as liveness, and
// nothing else touches the daemon on the LAN path (the phone fetches
// Metro directly). Polls fast while starting to narrate state
// transitions under the QR, then drops to keepalive cadence.
func watchExpoPreview(ctx context.Context, pv *daemonclient.PreviewClient, lastState string) error {
	if lastState == string(preview.StateReady) {
		fmt.Println("Dev server is already running — reattached.")
	}
	interval := previewStartPollInterval
	if lastState == string(preview.StateReady) {
		interval = previewKeepaliveInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st, err := pv.Status(tctx)
		cancel()
		if err != nil {
			continue // transient daemon hiccup; the next tick retries
		}
		if st.State == lastState {
			continue
		}
		switch st.State {
		case string(preview.StateReady):
			fmt.Println("Dev server is ready — scan the QR to open it.")
			ticker.Reset(previewKeepaliveInterval)
		case string(preview.StateFailed):
			printPreviewLogsTail(ctx, pv)
			return fmt.Errorf("dev server failed to start — output above")
		case string(preview.StateStopped):
			return fmt.Errorf("dev server was stopped outside this session")
		case string(preview.StateStarting):
			// Respawned under us (e.g. a phone-initiated start after a
			// failure) — narrate and go back to fast polls.
			fmt.Println("Dev server is starting…")
			ticker.Reset(previewStartPollInterval)
		}
		lastState = st.State
	}
}

// printPreviewLogsTail dumps the last chunk of dev-server output so a
// failed start is debuggable straight from the terminal.
func printPreviewLogsTail(ctx context.Context, pv *daemonclient.PreviewClient) {
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	logs, err := pv.Logs(tctx)
	if err != nil || len(logs) == 0 {
		return
	}
	const tailBytes = 2048
	if len(logs) > tailBytes {
		logs = logs[len(logs)-tailBytes:]
	}
	fmt.Printf("\n--- dev server output (tail) ---\n%s\n", logs)
}
