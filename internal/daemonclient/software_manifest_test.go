package daemonclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestSoftwareManifest_RetriesOn502 covers the user-visible bug: the
// gateway returns 502 "host unavailable" while a cold sprite warms up,
// and the next attempt succeeds. The error message we surface today
// even told the user "retry in a moment" — make sure we actually do.
func TestSoftwareManifest_RetriesOn502(t *testing.T) {
	// serial: mutates package-global softwareManifestBackoffBaseForTest
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if n < 3 {
			http.Error(w, "host unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"opencode": {"version": "0.1.0"}}`))
	}))
	defer srv.Close()

	c := NewTCPClient(srv.URL, "tok")

	// Drop the backoff base to keep the test fast; we still cover the
	// retry semantics. (The const is package-level; we mutate it via
	// a test-only path below.)
	oldBase := softwareManifestBackoffBaseForTest
	softwareManifestBackoffBaseForTest = 5 * time.Millisecond
	defer func() { softwareManifestBackoffBaseForTest = oldBase }()

	_, err := c.SoftwareManifest(context.Background())
	if err != nil {
		t.Fatalf("SoftwareManifest: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Errorf("call count = %d, want 3 (2 retries + success)", got)
	}
}

// TestSoftwareManifest_GivesUpAfterMaxAttempts confirms we don't
// retry forever — the user gets a clear error after the configured
// attempts elapse.
func TestSoftwareManifest_GivesUpAfterMaxAttempts(t *testing.T) {
	// serial: mutates package-global softwareManifestBackoffBaseForTest
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		http.Error(w, "host unavailable", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewTCPClient(srv.URL, "tok")

	oldBase := softwareManifestBackoffBaseForTest
	softwareManifestBackoffBaseForTest = 5 * time.Millisecond
	defer func() { softwareManifestBackoffBaseForTest = oldBase }()

	_, err := c.SoftwareManifest(context.Background())
	if err == nil {
		t.Fatal("expected error after persistent 502")
	}
	if got := atomic.LoadInt64(&calls); got != softwareManifestRetries {
		t.Errorf("call count = %d, want %d", got, softwareManifestRetries)
	}
}

// TestSoftwareManifest_NoRetryOnNon5xx makes sure a 4xx response
// surfaces immediately (these are configuration/auth errors that
// won't fix themselves).
func TestSoftwareManifest_NoRetryOnNon5xx(t *testing.T) {
	t.Parallel()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewTCPClient(srv.URL, "tok")

	_, err := c.SoftwareManifest(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("call count = %d, want 1 (no retry on 4xx)", got)
	}
}

func TestIsColdSpriteError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("daemon returned status 502: host unavailable"), true},
		{errors.New("daemon returned status 503"), true},
		{errors.New("daemon returned status 504"), true},
		{errors.New("daemon returned status 500"), false},
		{errors.New("daemon returned status 401"), false},
		{errors.New("host unavailable"), true},
		{errors.New("connection refused"), false},
		{context.DeadlineExceeded, false},
		{context.Canceled, false},
	}
	for _, c := range cases {
		if got := isColdSpriteError(c.err); got != c.want {
			t.Errorf("isColdSpriteError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
