package sharetunnel

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicTunnelURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "startup banner line",
			line: "2026-08-11T07:00:00Z INF |  https://lively-fox-rain.trycloudflare.com                                             |",
			want: "https://lively-fox-rain.trycloudflare.com",
		},
		{
			name: "bare origin",
			line: "https://a-b.trycloudflare.com",
			want: "https://a-b.trycloudflare.com",
		},
		{
			name: "requesting line has the domain but no origin",
			line: "2026-08-11T07:00:00Z INF Requesting new quick Tunnel on trycloudflare.com...",
			want: "",
		},
		{
			name: "unrelated https URL",
			line: "INF terms of service: https://www.cloudflare.com/terms/",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := publicTunnelURL(tc.line)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("publicTunnelURL(%q) = %q, %v; want %q", tc.line, got, ok, tc.want)
			}
		})
	}
}

func TestStartPublishesPublicURL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := stubCloudflared(t, dir, "#!/bin/sh\n"+
		`printf '%s\n' "$@" > `+argsFile+"\n"+
		"echo 'INF |  https://stub-words-here.trycloudflare.com  |' >&2\n"+
		"exec sleep 30\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tun, err := Start(ctx, bin, mustParseURL(t, "http://127.0.0.1:5173"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tun.Stop()

	if tun.PublicURL != "https://stub-words-here.trycloudflare.com" {
		t.Fatalf("PublicURL = %q", tun.PublicURL)
	}

	wantArgs := "tunnel\n--no-autoupdate\n--url\nhttp://127.0.0.1:5173\n--http-host-header\n127.0.0.1:5173\n"
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if string(args) != wantArgs {
		t.Fatalf("cloudflared args = %q, want %q", args, wantArgs)
	}

	tun.Stop()
	select {
	case <-tun.Done():
	default:
		t.Fatal("Stop returned but Done is not closed")
	}
}

func TestStartReportsEarlyExitWithOutput(t *testing.T) {
	t.Parallel()

	bin := stubCloudflared(t, t.TempDir(), "#!/bin/sh\n"+
		"echo 'ERR failed to request quick Tunnel: 429 Too Many Requests' >&2\n"+
		"exit 1\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Start(ctx, bin, mustParseURL(t, "http://127.0.0.1:5173"))
	if err == nil || !strings.Contains(err.Error(), "429 Too Many Requests") {
		t.Fatalf("Start: err = %v, want the process output surfaced", err)
	}
}

func TestStartTimesOutWhenNoURLAppears(t *testing.T) {
	t.Parallel()

	bin := stubCloudflared(t, t.TempDir(), "#!/bin/sh\n"+
		"echo 'INF Requesting new quick Tunnel on trycloudflare.com...' >&2\n"+
		"exec sleep 30\n")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Start(ctx, bin, mustParseURL(t, "http://127.0.0.1:5173"))
	if err == nil || !strings.Contains(err.Error(), "Requesting new quick Tunnel") {
		t.Fatalf("Start: err = %v, want timeout with output tail", err)
	}
	// Start reaps the process before returning; a hung child would ride
	// out its full sleep here.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Start took %v; child was not stopped on timeout", elapsed)
	}
}

func TestFindBinary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	if _, err := FindBinary(); err == nil || !strings.Contains(err.Error(), "brew install cloudflared") {
		t.Fatalf("FindBinary on empty PATH: err = %v, want install guidance", err)
	}

	want := stubCloudflared(t, dir, "#!/bin/sh\nexit 0\n")
	got, err := FindBinary()
	if err != nil || got != want {
		t.Fatalf("FindBinary = %q, %v; want %q", got, err, want)
	}
}

func stubCloudflared(t *testing.T, dir, script string) string {
	t.Helper()
	path := filepath.Join(dir, BinaryName)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub cloudflared: %v", err)
	}
	return path
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
