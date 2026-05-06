package daytona

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	hostclient "github.com/acksell/clank/internal/host/client"
	"github.com/acksell/clank/provisioner"
	"github.com/acksell/clank/internal/store"
)

// mustOpenStore opens an empty test store. Cleanup via t.Cleanup.
func mustOpenStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestNew_FailsFastOnMissingOptions pins the construction guards.
func TestNew_FailsFastOnMissingOptions(t *testing.T) {
	t.Parallel()
	s := mustOpenStore(t)
	cases := []struct {
		name string
		opts Options
	}{
		// SDKClient is nil + APIKey empty → SDK construction fails.
		{"missing-api-key", Options{HubBaseURL: "http://h", HubAuthToken: "t", Snapshot: "snap"}},
		{"missing-hub-url", Options{APIKey: "k", HubAuthToken: "t", Snapshot: "snap"}},
		{"missing-hub-token", Options{APIKey: "k", HubBaseURL: "http://h", Snapshot: "snap"}},
		{"missing-snapshot-and-image", Options{APIKey: "k", HubBaseURL: "http://h", HubAuthToken: "t"}},
		{"both-snapshot-and-image", Options{APIKey: "k", HubBaseURL: "http://h", HubAuthToken: "t", Snapshot: "snap", Image: "img"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(c.opts, s, nil); err == nil {
				t.Errorf("New(%+v) returned nil error", c.opts)
			}
		})
	}
}

// TestNew_RejectsNilStore enforces the precondition that a real store
// is wired in. Without it, persistence (the entire point of the
// provisioner) is silently broken.
func TestNew_RejectsNilStore(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{APIKey: "k", HubBaseURL: "http://h", HubAuthToken: "t", Snapshot: "snap"}, nil, nil); err == nil {
		t.Error("New with nil store returned nil error")
	}
}

// TestNew_RejectsReservedExtraEnv pins the guard against a user pref
// like CLANK_HUB_URL silently overriding launcher wiring.
func TestNew_RejectsReservedExtraEnv(t *testing.T) {
	t.Parallel()
	s := mustOpenStore(t)
	for _, key := range reservedSandboxEnv {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			opts := Options{
				APIKey: "k", HubBaseURL: "http://h", HubAuthToken: "t",
				Snapshot: "snap",
				ExtraEnv: map[string]string{key: "rogue-value"},
			}
			_, err := New(opts, s, nil)
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Errorf("want reserved-key error for %s, got %v", key, err)
			}
		})
	}
}

// TestSafeHostnameSuffix locks the hostname-suffix shape.
func TestSafeHostnameSuffix(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"sb-abc-123def456789":        "123def456789",
		"sandbox-aaa":                "aaa",
		"plain":                      "plain",
		"long-tail-abcdefghijklmnop": "abcdefghijkl",
	}
	for in, want := range cases {
		if got := safeHostnameSuffix(in); got != want {
			t.Errorf("safeHostnameSuffix(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestPreviewTokenInjector verifies the RoundTripper attaches the
// `x-daytona-preview-token` header on every outbound request.
func TestPreviewTokenInjector(t *testing.T) {
	t.Parallel()
	gotCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCh <- r.Header.Get("x-daytona-preview-token")
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)

	cli := &http.Client{Transport: &previewTokenInjector{token: "tkn"}}
	resp, err := cli.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if got := <-gotCh; got != "tkn" {
		t.Errorf("preview token header: got %q, want %q", got, "tkn")
	}
}

// TestPreviewTokenInjector_CloseIdleConnectionsDelegates pins the
// hostclient.HTTP.Close → injector → wrapped transport delegation.
func TestPreviewTokenInjector_CloseIdleConnectionsDelegates(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	base := &idlerRT{onClose: func() { called.Store(true) }}
	p := &previewTokenInjector{token: "x", wrapped: base}
	c := hostclient.NewHTTP("http://x", p)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !called.Load() {
		t.Error("hostclient.HTTP.Close should reach the wrapped transport via the injector")
	}
}

// idlerRT is a stub RoundTripper that records CloseIdleConnections calls.
type idlerRT struct {
	onClose func()
}

func (r *idlerRT) RoundTrip(*http.Request) (*http.Response, error) { panic("unused") }
func (r *idlerRT) CloseIdleConnections()                           { r.onClose() }

// TestWaitForHostReady_RetriesUntilStatusOK verifies the readiness
// probe keeps polling /status until the sandbox starts answering.
func TestWaitForHostReady_RetriesUntilStatusOK(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	var ready atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !ready.Load() && hits.Load() < 3 {
			http.Error(w, "Bad Gateway", 502)
			return
		}
		ready.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hostname":   "fake",
			"version":    "test",
			"started_at": time.Now(),
			"sessions":   0,
		})
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := hostclient.NewHTTP(srv.URL, http.DefaultTransport)
	if err := waitForHostReady(ctx, c, "sb-test"); err != nil {
		t.Fatalf("waitForHostReady: %v", err)
	}
	if hits.Load() < 3 {
		t.Errorf("expected at least 3 attempts before ready, got %d", hits.Load())
	}
}

// TestWaitForHostReady_TimesOutWithUnderlyingError makes sure a
// permanently-broken sandbox surfaces a useful message rather than
// just "context deadline exceeded".
func TestWaitForHostReady_TimesOutWithUnderlyingError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Bad Gateway", 502)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	c := hostclient.NewHTTP(srv.URL, http.DefaultTransport)
	err := waitForHostReady(ctx, c, "sb-broken")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "sb-broken") {
		t.Errorf("error should name the sandbox, got %q", err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "Bad Gateway") && !strings.Contains(err.Error(), "502") {
		t.Errorf("error should mention the underlying 502, got %q", err.Error())
	}
}

// TestCacheRoundTrip exercises the simple cache get/set/drop helpers.
// Edge case: cacheDrop must close the cached client to avoid leaking
// idle conns when a stale URL forces refresh.
func TestCacheRoundTrip(t *testing.T) {
	t.Parallel()
	p := &Provisioner{cache: map[string]*cachedHost{}}
	if got := p.cacheGet("u"); got != nil {
		t.Fatalf("empty cache should return nil, got %+v", got)
	}
	c := &cachedHost{hostID: "h-1"}
	p.cacheSet("u", c)
	if got := p.cacheGet("u"); got == nil || got.hostID != "h-1" {
		t.Fatalf("cacheGet after set: got %+v", got)
	}
	p.cacheDrop("u")
	if got := p.cacheGet("u"); got != nil {
		t.Fatalf("cacheGet after drop: got %+v want nil", got)
	}
}

// _ asserts at compile time that *Provisioner satisfies the
// provisioner.Provisioner interface. Catches accidental signature drift.
var _ provisioner.Provisioner = (*Provisioner)(nil)
