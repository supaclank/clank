package syncclient

import (
	"net/http"
	"testing"
)

// TestDefaultHTTPClient_HasResponseHeaderTimeout pins the contract that
// New() with no caller-supplied client gives back a transport with a
// bounded response-header timeout, so a stuck server can't hang the
// CLI/TUI indefinitely on presign / upload / download calls.
func TestDefaultHTTPClient_HasResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	c := defaultHTTPClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default client Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Fatal("default client Transport.ResponseHeaderTimeout is zero — calls can hang forever")
	}
}
