package main

import "testing"

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
