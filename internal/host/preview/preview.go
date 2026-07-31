// Package-level docs live in doc.go.
package preview

import (
	"sync"
	"time"
)

// Kind tags what flavor of dev server a Spec describes. Drives the
// client's render decision once the server is up: KindExpo is consumed
// by clank-mobile (QR + phone), KindWeb by `clank preview`'s browser
// flow, which fronts the dev server with the overlay-injecting proxy
// in internal/webpreview instead of printing a QR.
type Kind string

const (
	KindExpo Kind = "expo"
	KindWeb  Kind = "web"
)

// State is the lifecycle state of a running server. Status responses
// expose this verbatim so the mobile loading screen can drive its UI.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateFailed   State = "failed"
)

// Spec is the result of Detect: the recipe Manager uses to spawn the
// dev server. Internal to this package — never wire-serialized. If the
// shape ever escapes, that's the signal that v2's config-file work has
// landed and Spec should be reborn as a typed wire schema.
type Spec struct {
	// Kind identifies which client integration to use: KindExpo drives
	// the phone/QR flow, KindWeb the `clank preview` browser flow.
	Kind Kind

	// CmdTemplate is the argv template; PortToken occurrences are
	// replaced with the allocated port at spawn time.
	CmdTemplate []string

	// PortToken is the placeholder renderArgs substitutes. Detect
	// always emits clankyaml.PortPlaceholder — one token for
	// synthesized and user recipes alike, so a literal "%d" in a user
	// command or install string never gets mangled. Empty means "%d"
	// (internal and test recipes predating the config work).
	PortToken string

	// Dir is the repo-relative subdirectory the dev server runs in
	// (clank.yaml preview.dir). Empty means the workdir itself.
	Dir string

	// Installer records what installs dependencies, for the bootstrap
	// completion marker: a Packager name for synthesized installs, the
	// verbatim clank.yaml preview.install command for overrides, empty
	// when the spec has no install step (custom command without an
	// install). Drives the cross-packager node_modules wipe decision
	// (needsNodeModulesWipe).
	Installer string

	// RequiredTool is a binary that must be on PATH for the spawn to
	// make sense (the resolved package manager). Start fails fast,
	// citing ToolEvidence, when it's missing; empty skips the check.
	RequiredTool string

	// ToolEvidence is the human-readable reason RequiredTool is
	// required — the lockfile or package.json field that decided (see
	// ResolvePackager), or the user's saved choice. Empty for the
	// no-signal bun default.
	ToolEvidence string

	// ReadyProbe is the HTTP poll Manager runs after spawn to flip
	// State from Starting to Ready. Concrete contract beats stdout-
	// scanning: the probe only passes when the dev server is actually
	// serving traffic, vs. the print-then-bind race we had earlier
	// with substring matching.
	ReadyProbe ReadyProbe
}

// ReadyProbe describes the HTTP-readiness check. Manager.spawn polls
// http://127.0.0.1:<port><Path> every 200ms until the response is 200
// AND the body contains ExpectedSubstr (or always, if empty). The
// probe times out at the package default unless overridden via
// spawnRequest.ReadyTimeout.
//
// For Expo, Metro exposes /status returning "packager-status:running"
// — see detect.go.
type ReadyProbe struct {
	Path           string
	ExpectedSubstr string
}

// Status is the wire-format snapshot a status endpoint returns.
// Exported so the mux package can encode it without cross-cutting
// imports.
//
// StartedAt is a pointer so its absence (server not running) renders
// as JSON null instead of the time.Time zero value, which JSON-encodes
// as the misleading "0001-01-01T00:00:00Z".
//
// Token/URL/ExpiresAt are populated after the gateway registers the
// route — when the sprite runs without a gateway integration (laptop
// dev), they stay empty and clients fall back to status-only display.
type Status struct {
	Available   bool       `json:"available"`
	Kind        Kind       `json:"kind,omitempty"`
	ServiceName string     `json:"service_name,omitempty"`
	State       State      `json:"state"`
	Port        int        `json:"port,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	LastErr     string     `json:"last_err,omitempty"`

	// Token is the gateway-minted token for this preview's public URL.
	// Empty when the manager's GWClient is disabled (laptop dev).
	Token string `json:"token,omitempty"`

	// URL is the public preview URL — preview-<Token>.<root>. Empty
	// when Token is empty. Clients pass this verbatim to whatever
	// renders the bundle.
	URL string `json:"url,omitempty"`

	// ExpiresAt is when the gateway will stop honoring the token.
	// Re-register (re-call /preview/start) before this to refresh.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// running is Manager's in-memory record of a live server. The mutex
// protects state transitions (Starting → Ready → Stopped/Failed) and
// the lastTouch timestamp the idle-reaper reads.
type running struct {
	mu sync.Mutex

	spec        Spec
	serviceName string
	port        int
	state       State
	lastErr     string
	startedAt   time.Time
	lastTouch   time.Time

	// Gateway-registered route fields. Populated after a successful
	// GWClient.Register; empty when the gateway isn't wired (laptop
	// dev) or registration failed (logged + tolerated).
	token     string
	url       string
	expiresAt time.Time

	logs *ringBuf

	// pid is the spawned child's PID; pgid is its process group (set
	// by procgroup_unix.go right after Start). Both are zero when no
	// process is running.
	pid  int
	pgid int

	// done closes when the wait goroutine has reaped the child. Read
	// by Stop to know when SIGKILL is safe to skip and when to return.
	done chan struct{}

	// cancel tears down the spawn context (kills the process via the
	// CommandContext linkage if it's still alive when the manager is
	// asked to stop).
	cancel func()
}

// touch marks the record as recently used so the idle reaper spares
// it. Called on every control-plane read (Status, idempotent Start) —
// the CLI keepalive and phone polling are the liveness signal for LAN
// previews, whose Metro traffic never crosses the daemon.
func (r *running) touch() {
	r.mu.Lock()
	r.lastTouch = time.Now()
	r.mu.Unlock()
}

// snapshot copies the externally-visible fields under r.mu so callers
// can format JSON without holding a lock across I/O.
func (r *running) snapshot() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Status{
		Available:   true,
		Kind:        r.spec.Kind,
		ServiceName: r.serviceName,
		State:       r.state,
		Port:        r.port,
		LastErr:     r.lastErr,
		Token:       r.token,
		URL:         r.url,
	}
	if !r.startedAt.IsZero() {
		started := r.startedAt
		out.StartedAt = &started
	}
	if !r.expiresAt.IsZero() {
		exp := r.expiresAt
		out.ExpiresAt = &exp
	}
	return out
}

// stopWithGrace is the canonical teardown for a running record. Reads
// pgid under r.mu so it doesn't race with the wait goroutine's reset
// to 0 (the race detector caught this otherwise). Blocks until the
// process group is reaped.
//
// Callers (Manager.Stop, reaper, Shutdown, tests) must use this
// instead of calling stopProcessGroup directly with r.pgid.
func (r *running) stopWithGrace(grace time.Duration) {
	r.mu.Lock()
	pgid := r.pgid
	r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
	stopProcessGroup(pgid, r.done, grace)
}
