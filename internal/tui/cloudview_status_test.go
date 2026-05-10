package tui

import (
	"errors"
	"testing"

	"github.com/acksell/clank/internal/cloud"
	"github.com/acksell/clank/internal/config"
)

// TestCloudView_Status_DiskBaseline pins the rule that disk identity
// dominates: NotConfigured / Offline are returned regardless of any
// in-memory reachability state. Reaching the server doesn't matter if
// we don't know who we are.
//
// Not t.Parallel: HOME is process-global. Same constraint as
// internal/config tests.
func TestCloudView_Status_DiskBaseline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	m := cloudView{hasCalled: true}
	if got := m.Status(); got != cloudStatusNotConfigured {
		t.Fatalf("no prefs: got %v, want NotConfigured", got)
	}

	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.Cloud = &config.CloudPreference{CloudURL: "https://example.test"}
	}); err != nil {
		t.Fatalf("UpdatePreferences: %v", err)
	}
	// cloud_url set, but no token → Offline regardless of in-memory state.
	m.lastCallErr = nil
	if got := m.Status(); got != cloudStatusOffline {
		t.Fatalf("url only: got %v, want Offline", got)
	}
	m.lastCallErr = errors.New("boom")
	if got := m.Status(); got != cloudStatusOffline {
		t.Fatalf("url only with error: got %v, want Offline", got)
	}
}

// TestCloudView_Status_ReachabilityAxis pins the rule that once disk
// identity is satisfied (token present, unexpired), Status flips
// between Checking → Online | Unavailable based on the most recent
// server call. ErrUnauthorized is identity, not reachability, and
// should fall through to the Offline branch (clearSession would have
// already wiped the token in production).
func TestCloudView_Status_ReachabilityAxis(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := config.UpdatePreferences(func(p *config.Preferences) {
		p.Cloud = &config.CloudPreference{
			CloudURL:    "https://example.test",
			AccessToken: "tok-abc",
			// ExpiresAt: 0 means "no expiry tracked" → cloudTokenExpired returns false.
		}
	}); err != nil {
		t.Fatalf("UpdatePreferences: %v", err)
	}

	m := cloudView{}
	if got := m.Status(); got != cloudStatusChecking {
		t.Errorf("token, no call yet: got %v, want Checking", got)
	}

	m.hasCalled = true
	m.lastCallErr = nil
	if got := m.Status(); got != cloudStatusOnline {
		t.Errorf("call ok: got %v, want Online", got)
	}

	m.lastCallErr = errors.New("connection refused")
	if got := m.Status(); got != cloudStatusUnavailable {
		t.Errorf("transport error: got %v, want Unavailable", got)
	}

	m.lastCallErr = cloud.ErrUnauthorized
	if got := m.Status(); got != cloudStatusOffline {
		t.Errorf("ErrUnauthorized: got %v, want Offline (defensive)", got)
	}
}
