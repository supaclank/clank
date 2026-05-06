// Package provisioner defines the contract between the gateway/hub
// layers and the cloud-side machinery that owns persistent per-user
// hosts.
//
// A Provisioner is responsible for ensuring that a given user has
// exactly one persistent host on its provider (Daytona sandbox, Fly.io
// Sprite, k8s pod, …) and for surfacing the URL/token the gateway
// uses to reach it. Lifecycle is persistence-first: EnsureHost is
// idempotent (get-or-create-or-wake), SuspendHost is cooperative
// (compute-saving, state-preserving), and DestroyHost is the only
// path that throws away workspace state.
//
// Concrete implementations live in subpackages: daytona, flyio, local.
// Each returns the same HostRef shape so upper layers stay
// provider-agnostic.
package provisioner

import (
	"context"
	"net/http"
)

// HostRef carries everything the gateway needs to reach a user's host.
// It is the return value of EnsureHost and is safe to cache for the
// duration of one daemon lifetime, but stale values must be re-resolved
// after a /status probe failure (URLs may rotate across stop/start
// cycles).
type HostRef struct {
	// HostID is the store-internal UUID of this host record. Used by
	// the caller to invoke SuspendHost or DestroyHost later.
	HostID string

	// URL is the base URL the gateway will proxy to. For Daytona this
	// is the preview URL; for Sprites it is the public sprite URL; for
	// the local-subprocess provider it is http://127.0.0.1:<port>.
	URL string

	// Transport is the fully-wired http.RoundTripper that injects
	// every header required to reach this host: the universal
	// capability-token (Authorization: Bearer) plus any provider-edge
	// auth (e.g. Daytona's x-daytona-preview-token). Consumers
	// construct an HTTP client as `hostclient.NewHTTP(ref.URL,
	// ref.Transport)` and stay agnostic to the auth chain shape.
	//
	// Non-nil. The provisioner builds the chain and validates its
	// pieces before returning.
	Transport http.RoundTripper

	// AuthToken is the bearer token baked into the host's clank-host
	// require-bearer middleware. Surfaced separately from Transport
	// for storage/logging purposes; the same value is already wired
	// into Transport for outbound requests.
	AuthToken string

	// AutoWake indicates the provider's URL wakes the underlying
	// compute on incoming traffic without an explicit API call. True
	// for Sprites; false for Daytona. The gateway uses this to decide
	// whether a probe failure means "stale URL, re-resolve" (false) or
	// "edge will wake on retry" (true).
	// todo(ae): Leaky abstraction. The provisioner should instead always implement auto-wake.
	AutoWake bool

	// Hostname is the stable identifier surfaced to upper layers
	// (session metadata, hub catalog). Stable across stop/resume of
	// the same underlying host.
	Hostname string
}

// Provisioner is the interface the gateway/hub uses to obtain and
// manage a user's persistent host. Implementations MUST be safe for
// concurrent use by multiple goroutines: callers will issue overlapping
// EnsureHost requests for the same userID and expect them to converge
// on the same single host.
type Provisioner interface {
	// EnsureHost resolves (or creates, or wakes) the persistent host
	// for userID. Idempotent across calls within a daemon lifetime AND
	// across daemon restarts (state survives via the underlying
	// store). Returns a HostRef pointing at a host that has just
	// passed a readiness probe.
	EnsureHost(ctx context.Context, userID string) (HostRef, error)

	// SuspendHost issues a cooperative suspend on the underlying
	// compute (Daytona stop, Sprite hibernate, etc.) so the user's
	// workspace stops billing for compute. State is preserved; a
	// subsequent EnsureHost wakes it.
	//
	// Idempotent: suspending an already-stopped host is not an error.
	SuspendHost(ctx context.Context, hostID string) error

	// DestroyHost permanently deletes the underlying compute and
	// removes the store row. Used for explicit account/workspace
	// teardown. Out-of-band deletion at the provider is detected
	// inside EnsureHost (NotFound from Get) and handled there; callers
	// don't need to invoke DestroyHost for that case.
	DestroyHost(ctx context.Context, hostID string) error
}

// Closer is implemented by provisioners that own background work or
// SDK clients that need explicit cleanup at daemon shutdown. Optional;
// callers should type-assert.
type Closer interface {
	Stop()
}
