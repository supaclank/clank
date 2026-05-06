package flyio

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestIsSpritesEdge404 distinguishes the Sprites edge "no service
// bound" page from a real 404 the host might return. The edge page
// has a recognizable <title>; clank-host's own 404s never look like
// that.
func TestIsSpritesEdge404(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"edge-404", http.StatusNotFound, `<!DOCTYPE html><html><head><title>404 | Sprites</title></head></html>`, true},
		{"real-404", http.StatusNotFound, `{"error":"page not found"}`, false},
		{"200-with-edge-title", http.StatusOK, `<title>404 | Sprites</title>`, false},
		{"500-edge", http.StatusInternalServerError, `<title>404 | Sprites</title>`, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := isSpritesEdge404(c.status, []byte(c.body)); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestWaitForSpriteReady_ReturnsOnceClankHostBinds is the headline
// regression: the sprite serves the edge 404 page until clank-host
// binds its port, then starts proxying. waitForSpriteReady polls
// until that flip happens, so EnsureHost doesn't return a HostRef
// pointing at a still-404'ing endpoint.
func TestWaitForSpriteReady_ReturnsOnceClankHostBinds(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			// First two probes: edge 404.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<!DOCTYPE html><title>404 | Sprites</title>`)
			return
		}
		// Third probe: real host responding.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := waitForSpriteReady(ctx, srv.URL, http.DefaultTransport, log.Default()); err != nil {
		t.Fatalf("waitForSpriteReady: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got < 3 {
		t.Errorf("expected at least 3 probes, got %d", got)
	}
}

// TestWaitForSpriteReady_RealHost404IsNotEdge — if the host itself
// genuinely 404s the path, that means the mux is up (just disagrees
// about the URL). The probe should treat that as "ready", not keep
// polling until timeout.
func TestWaitForSpriteReady_RealHost404IsNotEdge(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"unknown route"}`)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := waitForSpriteReady(ctx, srv.URL, http.DefaultTransport, log.Default()); err != nil {
		t.Fatalf("waitForSpriteReady should accept a real-host 404 as ready: %v", err)
	}
}

// TestWaitForSpriteReady_TimesOutWithSnippet pins that a perpetual
// edge 404 surfaces a useful error message, not a generic "context
// deadline exceeded". The snippet helps the operator distinguish
// edge 404 from real-host 404 in production logs.
func TestWaitForSpriteReady_TimesOutWithSnippet(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<title>404 | Sprites</title>perpetually unbound`)
	}))
	t.Cleanup(srv.Close)

	// Use a short ctx — waitForSpriteReady's internal 60s deadline
	// is too long for a unit test; we cancel via ctx instead.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	err := waitForSpriteReady(ctx, srv.URL, http.DefaultTransport, log.Default())
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention last status; got %v", err)
	}
}
