package tui

// cloud_status.go — single source of truth for "what should the sidebar
// say about the cloud connection?". Two independent axes are folded into
// a single 5-state enum:
//
//   - identity   : do we have a (locally-valid) token on disk?
//   - reachability: is the cloud actually responding?
//
// The disk-derived baseline (loadCloudAuthStatus) only knows about
// identity. The cloudView mixes in reachability via cloudView.Status().

import (
	"github.com/acksell/clank/internal/config"
)

// cloudAuthStatus reflects the user's cloud connection state.
type cloudAuthStatus int

const (
	// cloudStatusNotConfigured: no cloud_url is set; nothing to connect to.
	cloudStatusNotConfigured cloudAuthStatus = iota
	// cloudStatusOffline: cloud_url is configured but no usable access
	// token is on disk (missing or expired).
	cloudStatusOffline
	// cloudStatusChecking: token present on disk but we haven't seen a
	// server response yet (cold start, or a /me is in flight). Rendered
	// with a spinner so the user knows we're waiting on a verdict.
	cloudStatusChecking
	// cloudStatusOnline: token present and the most recent server call
	// succeeded.
	cloudStatusOnline
	// cloudStatusUnavailable: token present but the most recent server
	// call failed for non-auth reasons (network, 5xx). Identity is still
	// believed-valid; only reachability is the problem.
	cloudStatusUnavailable
)

// loadCloudAuthStatus inspects persisted preferences and returns the
// disk-derived baseline state. It performs no network I/O. Server
// reachability is layered on top by cloudView.Status().
func loadCloudAuthStatus() cloudAuthStatus {
	prefs, err := config.LoadPreferences()
	if err != nil {
		return cloudStatusNotConfigured
	}
	p := prefs.ActiveRemote()
	if p == nil || p.GatewayURL == "" {
		return cloudStatusNotConfigured
	}
	if p.AccessToken == "" || cloudTokenExpired(p) {
		return cloudStatusOffline
	}
	return cloudStatusOnline
}
