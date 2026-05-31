package clankcli

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{
		512:                    "512 B",
		1024:                   "1.0 KB",
		1536:                   "1.5 KB",
		58 * 1024 * 1024:       "58.0 MB",
		3 * 1024 * 1024 * 1024: "3.0 GB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestRemoteLabel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"http://localhost:7878":         "localhost:7878",
		"https://gateway.example.com":   "gateway.example.com",
		"https://gw.example.com:8443/x": "gw.example.com:8443",
	}
	for raw, want := range cases {
		if got := remoteLabel(raw); got != want {
			t.Errorf("remoteLabel(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestRenderBar(t *testing.T) {
	t.Parallel()
	empty := stripANSI(renderBar(0, 24))
	if strings.ContainsAny(empty, "█▓") || !strings.Contains(empty, "░") {
		t.Errorf("0%% bar should be all empty: %q", empty)
	}
	full := stripANSI(renderBar(1, 24))
	if strings.ContainsAny(full, "░▓") || !strings.Contains(full, "█") {
		t.Errorf("100%% bar should be all full: %q", full)
	}
	// A partial fill shows full blocks, a single ▓ transition, and empties.
	partial := stripANSI(renderBar(0.5, 23)) // 11.5 cells → 11 full + ▓
	for _, want := range []string{"█", "▓", "░"} {
		if !strings.Contains(partial, want) {
			t.Errorf("partial bar %q missing %q", partial, want)
		}
	}
}

// TestPushLine pins the status-line content: spinner + phase + remote
// always present; the bar + bytes appear only once a size is known.
func TestPushLine(t *testing.T) {
	t.Parallel()

	noSize := stripANSI(pushLine("⠋", "Saving checkpoint", "localhost:7878", 0, 0))
	for _, want := range []string{"⠋", "Saving checkpoint", "localhost:7878"} {
		if !strings.Contains(noSize, want) {
			t.Errorf("size-unknown line %q missing %q", noSize, want)
		}
	}
	if strings.ContainsAny(noSize, "█░") {
		t.Errorf("size-unknown line should have no bar: %q", noSize)
	}

	withSize := stripANSI(pushLine("⠋", "Uploading", "localhost:7878", 50, 100))
	for _, want := range []string{"Uploading", "█", "░", "50 B / 100 B"} {
		if !strings.Contains(withSize, want) {
			t.Errorf("uploading line %q missing %q", withSize, want)
		}
	}
}

// TestPushUI_ObserverNilSafe pins that a nil *pushUI (the non-interactive
// path) is a safe no-op — PushCheckpoint always gets a usable observer.
func TestPushUI_ObserverNilSafe(t *testing.T) {
	t.Parallel()
	var u *pushUI
	u.start()
	u.Phase("x")
	u.UploadSized(10)
	u.UploadProgress(5)
	u.finish() // must not panic
}

// TestPushUI_ConcurrentRenderNoRace exercises the render-loop goroutine
// reading the fields the push goroutine writes — guards against a data
// race in the live status line (run with -race).
func TestPushUI_ConcurrentRenderNoRace(t *testing.T) {
	t.Parallel()
	ui := newPushUI(io.Discard, "localhost:7878")
	ui.start()
	for i := 0; i <= 40; i++ {
		ui.UploadSized(40)
		ui.UploadProgress(int64(i))
		ui.Phase("Uploading")
		time.Sleep(5 * time.Millisecond) // let the 100ms ticker fire a few draws
	}
	ui.finish()
}
