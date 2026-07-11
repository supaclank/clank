package main

import (
	"io"
	"log"
	"strings"
	"testing"
)

// TestRequireLoopbackTCP pins the startup safety guard: a non-loopback
// TCP bind without a token must fail fast. CR (#3181612371) flagged
// the empty-token middleware no-op as fail-open; the alt-fix here
// addresses the same threat at process start instead of per-request.
func TestRequireLoopbackTCP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		listen  string
		wantErr bool
	}{
		{"unix-socket-skipped", "unix:///tmp/x.sock", false},
		{"loopback-ipv4", "tcp://127.0.0.1:8080", false},
		{"loopback-ipv6", "tcp://[::1]:8080", false},
		{"loopback-localhost", "tcp://localhost:8080", false},
		{"auto-port-loopback", "tcp://127.0.0.1:0", false},
		{"all-interfaces-rejected", "tcp://0.0.0.0:8080", true},
		{"empty-host-rejected", "tcp://:8080", true},
		{"public-ip-rejected", "tcp://1.2.3.4:8080", true},
		{"ipv6-wildcard-rejected", "tcp://[::]:8080", true},
		{"malformed-rejected", "tcp://not-a-valid-host:port", true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := requireLoopbackTCP(c.listen)
			if c.wantErr && err == nil {
				t.Errorf("requireLoopbackTCP(%q) returned nil; want error", c.listen)
			}
			if !c.wantErr && err != nil {
				t.Errorf("requireLoopbackTCP(%q) returned %v; want nil", c.listen, err)
			}
		})
	}
}

// TestBuildNotifierProvider pins the misconfig fast-fails so an
// operator who set --notifier-provider=webhook without the URL or
// token (or with both) gets a startup error instead of a silently
// broken delivery path. Empty token used to slip through, producing
// unauth'd POSTs that every dispatcher rejects with 401.
func TestBuildNotifierProvider(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		provider    string
		url         string
		token       string
		wantErr     bool
		wantErrSubs string
	}{
		{name: "none-skips", provider: "none"},
		{name: "noop-builds", provider: "noop"},
		{name: "webhook-happy", provider: "webhook", url: "https://x", token: "clnk_a"},
		{name: "webhook-missing-url", provider: "webhook", token: "clnk_a", wantErr: true, wantErrSubs: "--notifier-webhook-url"},
		{name: "webhook-missing-token", provider: "webhook", url: "https://x", wantErr: true, wantErrSubs: "--notifier-webhook-token"},
		{name: "webhook-missing-both-reports-url-first", provider: "webhook", wantErr: true, wantErrSubs: "--notifier-webhook-url"},
		{name: "unknown-provider", provider: "carrier-pigeon", wantErr: true, wantErrSubs: "unknown --notifier-provider"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildNotifierProvider(c.provider, c.url, c.token, nil)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if c.wantErrSubs != "" && !strings.Contains(err.Error(), c.wantErrSubs) {
					t.Errorf("error = %q, want substring %q", err.Error(), c.wantErrSubs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBuildKeepaliveListener(t *testing.T) {
	t.Parallel()
	shutdown := func() {}
	cases := []struct {
		name        string
		provider    string
		wantNil     bool
		wantErr     bool
		wantErrSubs string
	}{
		{name: "none-skips", provider: "none", wantNil: true},
		{name: "noop-builds", provider: "noop"},
		{name: "sprites-builds", provider: "sprites"},
		{name: "exit-builds", provider: "exit"},
		{name: "unknown-provider", provider: "carrier-pigeon", wantErr: true, wantErrSubs: "unknown --keepalive-provider"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			l, err := buildKeepaliveListener(c.provider, shutdown, log.New(io.Discard, "", 0))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if c.wantErrSubs != "" && !strings.Contains(err.Error(), c.wantErrSubs) {
					t.Errorf("error = %q, want substring %q", err.Error(), c.wantErrSubs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantNil != (l == nil) {
				t.Fatalf("listener nil = %v, want %v", l == nil, c.wantNil)
			}
		})
	}
}
