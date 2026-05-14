package agent

import (
	"context"
	"sync"
	"time"
)

// softwareManifestProbeTimeout bounds the FIRST /software-manifest
// probe. Independent of any caller's ctx so a short-deadline first
// request can't poison the process-global cache with an empty
// manifest (sync.Once.Do runs exactly once whether the probe
// succeeded or was canceled).
const softwareManifestProbeTimeout = 5 * time.Second

// SoftwareInfo describes one tool clank-host knows about. Version
// is empty when the tool isn't installed (or failed to respond to
// --version) — callers should treat empty as "unavailable" rather
// than treating absence of a record. Future fields (path, install
// method, install_time, etc.) can be added without breaking the
// wire shape.
type SoftwareInfo struct {
	Version string `json:"version"`
}

// SoftwareManifest is what GET /software-manifest returns. Today
// only opencode is meaningfully populated; claude and clank-host
// fields are reserved for future expansion. JSON keys are
// snake_case to match the rest of clank's wire conventions.
type SoftwareManifest struct {
	OpenCode SoftwareInfo `json:"opencode"`
	// Reserved for future expansion:
	//   Claude    SoftwareInfo `json:"claude"`
	//   ClankHost SoftwareInfo `json:"clank_host"`
}

// softwareManifestCache stores a probed manifest once per
// clank-host process lifetime. Subsequent calls return in
// nanoseconds instead of paying opencode's JS startup cost on
// every request. The cache is invalidated only by restarting
// clank-host — which is also when the embedded clank-host binary
// gets refreshed (see flyio provisioner's hashSidecar mechanic),
// so the cached opencode version going stale relative to a
// re-install is impossible by construction.
var (
	softwareManifestOnce sync.Once
	softwareManifest     SoftwareManifest
)

// GetSoftwareManifest returns the probed manifest, computing it
// lazily on first call. Concurrent first-callers serialize on
// sync.Once; once cached, reads are lock-free.
//
// ctx is accepted for symmetry with cancellable callers but is
// NOT plumbed into the probe — the probe runs on a private
// softwareManifestProbeTimeout context so a canceled first
// request can't permanently cache an empty manifest. If you
// need an uncached probe (e.g. to detect an out-of-band opencode
// upgrade), the right answer is to restart clank-host.
func GetSoftwareManifest(ctx context.Context) SoftwareManifest {
	_ = ctx
	softwareManifestOnce.Do(func() {
		probeCtx, cancel := context.WithTimeout(context.Background(), softwareManifestProbeTimeout)
		defer cancel()
		softwareManifest = probeSoftwareManifest(probeCtx)
	})
	return softwareManifest
}

// probeSoftwareManifest runs every tool's --version equivalent
// and assembles the manifest. Tool failures are silent: a tool
// that errors or doesn't exist is reported as Version="".
// Callers must treat empty Version as "tool not available
// here", NOT as "manifest probe failed."
func probeSoftwareManifest(ctx context.Context) SoftwareManifest {
	m := SoftwareManifest{}
	if v, err := OpenCodeVersion(ctx); err == nil {
		m.OpenCode = SoftwareInfo{Version: v}
	}
	return m
}
