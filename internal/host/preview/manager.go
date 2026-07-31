package preview

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Default lifecycle timers. Bumpable via Options for tests.
const (
	DefaultIdleTimeout = 15 * time.Minute
	DefaultStopGrace   = 3 * time.Second
	reaperInterval     = 1 * time.Minute

	// gwRevokeTimeout caps best-effort Revoke calls during teardown.
	// Short on purpose — a sluggish gateway shouldn't stall Stop or
	// Shutdown beyond the user-visible response window.
	gwRevokeTimeout = 5 * time.Second
)

// ErrNotPreviewable is returned by Start when Detect found nothing to
// spawn. The mux handler maps it to a 404 with a structured code so
// the mobile UI can fall back to hiding the button.
var ErrNotPreviewable = errors.New("preview: worktree is not previewable")

// ErrNotRunning is returned by Stop when no server exists for the
// worktree. Mapped to 404.
var ErrNotRunning = errors.New("preview: no preview server running for worktree")

// ErrBootstrapBusy is returned by Start when another preview is
// mid-bootstrap (wipe/install) in the same directory under a
// different registry key, so this start can't safely proceed yet.
// Retryable: the client should re-issue Start shortly.
var ErrBootstrapBusy = errors.New("preview: a preview is already installing in this folder — retry shortly")

// Options configures a Manager. Each field is optional with a sensible
// default — pass Options{} for a bare manager.
type Options struct {
	// IdleTimeout is how long a running server can go without proxy
	// traffic before the reaper stops it. Zero uses DefaultIdleTimeout.
	IdleTimeout time.Duration

	// StopGrace is the SIGTERM→SIGKILL window. Zero uses DefaultStopGrace.
	StopGrace time.Duration

	// Log is the logger; nil falls back to the default logger.
	Log *log.Logger

	// GWClient mints + revokes public tokens with the gateway. Nil
	// (or a disabled client) leaves Status.Token/URL empty — useful
	// for laptop dev where there's no gateway to register with.
	GWClient *GWClient

	// PackagerPolicy selects how detected frameworks get their
	// installer (see the PackagerPolicy consts). The zero value
	// behaves as reuse-project; the cloud machine's clank-host opts
	// into always-bun via its flag.
	PackagerPolicy PackagerPolicy
}

// serviceKey is the registry key. Two services on the same worktree
// (e.g. expo + a backend dev server in the future) live as distinct
// entries; today's caller always passes ServiceName = "default".
type serviceKey struct {
	WorktreeID  string
	ServiceName string
}

// Manager owns the per-(worktree, service) dev-server registry and
// exposes the lifecycle (Start/Stop/Status). One Manager lives on
// each host.Service.
type Manager struct {
	idleTimeout    time.Duration
	stopGrace      time.Duration
	log            *log.Logger
	gw             *GWClient
	packagerPolicy PackagerPolicy

	mu       sync.Mutex
	servers  map[serviceKey]*running
	closed   bool
	reaperWG sync.WaitGroup
	stopCh   chan struct{}

	// bootMu guards bootLeases: one mutex per canonical workdir,
	// serializing the bootstrap phase (wipe decision → install →
	// marker) across everything that might touch that directory —
	// concurrent services, and the same folder arriving under
	// different registry keys (worktree ID vs. folder slug). The
	// server registry above stays keyed by (worktree, service); this
	// is a second, directory-scoped coordination layer.
	bootMu     sync.Mutex
	bootLeases map[string]*sync.Mutex

	// bgCtx is the manager-scoped lifetime context handed to spawn.
	// Distinct from the HTTP request ctx because the request ends as
	// soon as Start writes its response — using the request ctx would
	// cancel exec.CommandContext and SIGKILL Metro instantly.
	bgCtx    context.Context
	bgCancel context.CancelFunc
}

// New constructs and starts a Manager. Call Shutdown to release the
// reaper goroutine.
func New(opts Options) *Manager {
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.StopGrace == 0 {
		opts.StopGrace = DefaultStopGrace
	}
	if opts.Log == nil {
		opts.Log = log.Default()
	}
	bgCtx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		idleTimeout:    opts.IdleTimeout,
		stopGrace:      opts.StopGrace,
		log:            opts.Log,
		gw:             opts.GWClient,
		packagerPolicy: opts.PackagerPolicy,
		servers:        make(map[serviceKey]*running),
		bootLeases:     make(map[string]*sync.Mutex),
		stopCh:         make(chan struct{}),
		bgCtx:          bgCtx,
		bgCancel:       cancel,
	}
	m.reaperWG.Add(1)
	go m.reaperLoop()
	return m
}

// Start spawns the dev server for (worktreeID, serviceName) and
// returns its Status. Idempotent — a second Start for the same
// (wid, service) returns the existing server's snapshot without
// re-spawning.
//
// After the spawn passes readiness, Start calls GWClient.Register to
// mint the public token + URL and stores those on the running record
// so subsequent Status calls surface them. When GWClient is nil or
// disabled, Status.Token/URL stay empty (laptop dev path).
//
// Returns ErrNotPreviewable when Detect found no recognizable framework.
func (m *Manager) Start(ctx context.Context, worktreeID, workDir, serviceName string) (Status, error) {
	spec, err := Detect(workDir, m.packagerPolicy)
	if err != nil {
		return Status{}, fmt.Errorf("detect: %w", err)
	}
	if spec == nil {
		return Status{}, ErrNotPreviewable
	}
	// Fail before spawning, with the detection evidence in hand — a
	// missing binary surfacing as a cryptic mid-bootstrap shell error
	// would hide the actual fix from the user.
	if spec.RequiredTool != "" {
		if _, lookErr := exec.LookPath(spec.RequiredTool); lookErr != nil {
			why := spec.ToolEvidence
			if why == "" {
				why = "clank's default installer"
			}
			return Status{}, fmt.Errorf("preview: this project uses %s (%s) — install %s to continue", spec.RequiredTool, why, spec.RequiredTool)
		}
	}
	return m.startWithSpec(ctx, worktreeID, workDir, serviceName, *spec, 0)
}

// startWithSpec is the lock+spawn+register core that Start wraps.
// Split out so tests can drive it with a custom Spec (and per-test
// ReadyTimeout) without mutating package-level vars in parallel.
//
// readyTimeout==0 falls through to the package default inside spawn.
func (m *Manager) startWithSpec(ctx context.Context, worktreeID, workDir, serviceName string, spec Spec, readyTimeoutOverride time.Duration) (Status, error) {
	if worktreeID == "" {
		return Status{}, fmt.Errorf("preview: worktree id is required")
	}
	if workDir == "" {
		return Status{}, fmt.Errorf("preview: work dir is required")
	}
	if serviceName == "" {
		return Status{}, fmt.Errorf("preview: service name is required")
	}

	key := serviceKey{WorktreeID: worktreeID, ServiceName: serviceName}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Status{}, fmt.Errorf("preview: manager is shut down")
	}
	if existing, ok := m.servers[key]; ok {
		snap := existing.snapshot()
		// Only honor live records. A Failed or Stopped entry comes from
		// the wait goroutine seeing the child die — without eviction
		// here, the user would be stuck with the stale snapshot forever
		// (no path to respawn except an explicit /stop first).
		if snap.State == StateReady || snap.State == StateStarting {
			m.mu.Unlock()
			existing.touch()
			return snap, nil
		}
		delete(m.servers, key)
	}
	m.mu.Unlock()

	// Take the directory's bootstrap lease before anything destructive
	// is decided: a wipe chosen by one spawn must never race another's
	// install in the same workdir. TryLock, not Lock — an install can
	// run for minutes and Start must stay non-blocking. On contention,
	// a short poll usually resolves the common case (a same-key
	// duplicate that loses the ms-wide race above finds the winner's
	// published record); a genuinely different key bootstrapping the
	// same folder gets a retryable ErrBootstrapBusy instead of a
	// request held across the whole install.
	lease := m.workdirLease(workDir)
	if !lease.TryLock() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			m.mu.Lock()
			existing, ok := m.servers[key]
			m.mu.Unlock()
			if ok {
				existing.touch()
				return existing.snapshot(), nil
			}
			if lease.TryLock() {
				goto leased
			}
			time.Sleep(50 * time.Millisecond)
		}
		return Status{}, ErrBootstrapBusy
	}
leased:
	// Every failure path below must release; success hands the lease
	// to a watcher that releases when the bootstrap phase ends (the
	// record leaves StateStarting or the child exits).
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(lease.Unlock) }

	// Allocate the listen port BEFORE registering so the gateway can
	// reserve a route to it AND we can thread the resulting public URL
	// into Metro's EXPO_PACKAGER_PROXY_URL. Order matters: Metro reads
	// the env var at startup, so we have to know the URL before spawn.
	port, err := allocatePort()
	if err != nil {
		release()
		return Status{}, fmt.Errorf("allocate port: %w", err)
	}

	// Best-effort gateway register. Failures are logged but don't
	// fail Start — Status surfaces empty Token/URL and Metro spawns
	// without the PROXY_URL override (laptop dev path: no gateway).
	regCtx, cancel := context.WithTimeout(ctx, gwClientTimeout)
	regResp, regErr := m.gw.Register(regCtx, RegisterRequest{
		WorktreeID:   worktreeID,
		ServiceName:  serviceName,
		InternalPort: port,
	})
	cancel()
	if regErr != nil {
		m.log.Printf("preview: gateway register for %s/%s failed (non-fatal): %v", worktreeID, serviceName, regErr)
	}

	// Prepare the guest-side preview runtime (Layer A): write the Metro shim +
	// runtime to a temp dir (NOT the user's repo) and hand the paths to spawn,
	// which preloads the shim via NODE_OPTIONS=--require so it injects the
	// runtime into every guest bundle in-memory. Best-effort — on failure the
	// preview still runs (no guest-side suppression; the clank-mobile host still
	// hides the native redbox). Expo-only; Detect today emits only KindExpo.
	var shimRequirePath, runtimePath string
	if spec.Kind == KindExpo {
		if sp, rp, ierr := ensurePreviewShim(); ierr != nil {
			m.log.Printf("preview: prepare runtime shim for %s/%s failed (non-fatal): %v", worktreeID, serviceName, ierr)
		} else {
			shimRequirePath, runtimePath = sp, rp
			m.log.Printf("preview: runtime shim ready (NODE_OPTIONS=--require %s)", sp)
		}
	}

	// Use the manager's background context, NOT the caller's. spawn
	// wires this to exec.CommandContext, and the caller's ctx (the
	// HTTP request context in production) gets canceled the moment
	// Start writes its response — that would SIGKILL Metro before it
	// printed a single line. The workdir bootstrap lease taken above
	// is what keeps this spawn's wipe/install from racing another
	// start in the same directory.
	r, err := spawn(m.bgCtx, spawnRequest{
		WorkDir:         workDir,
		Spec:            spec,
		ServiceName:     serviceName,
		Port:            port,
		PublicURL:       regResp.URL,
		ReadyTimeout:    readyTimeoutOverride,
		ShimRequirePath: shimRequirePath,
		RuntimePath:     runtimePath,
	})
	if err != nil {
		release()
		// Spawn failed — tear down the orphan route the gateway just
		// registered (if any) so we don't leave dangling tokens.
		if regErr == nil {
			m.revokeBestEffort(worktreeID, serviceName)
		}
		return Status{}, fmt.Errorf("spawn: %w", err)
	}
	if regErr == nil {
		r.mu.Lock()
		r.token = regResp.Token
		r.url = regResp.URL
		r.expiresAt = regResp.ExpiresAt
		r.mu.Unlock()
	}

	// Publish in StateStarting before blocking on readiness so a
	// concurrent retried Start sees the in-flight record and returns
	// early instead of spawning a duplicate Metro instance.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		r.stopWithGrace(m.stopGrace)
		release()
		if regErr == nil {
			m.revokeBestEffort(worktreeID, serviceName)
		}
		return Status{}, fmt.Errorf("preview: manager is shut down")
	}
	if existing, ok := m.servers[key]; ok {
		// Lost the race — another concurrent Start already published.
		// Keep theirs, discard ours.
		snap := existing.snapshot()
		m.mu.Unlock()
		r.stopWithGrace(m.stopGrace)
		release()
		if regErr == nil {
			// TODO(ai-review): revoke here uses (worktreeID, serviceName) which may evict the winner's live route if the gateway resolves by service pair rather than token. https://github.com/Acksell/clank/pull/36#discussion_r3324139755
			m.revokeBestEffort(worktreeID, serviceName)
		}
		m.log.Printf("preview: discarded duplicate spawn for %s/%s", worktreeID, serviceName)
		return snap, nil
	}
	m.servers[key] = r
	m.mu.Unlock()

	// Hand the lease to a watcher: the bootstrap phase is over when
	// the record leaves StateStarting (probe passed or startup failed)
	// or the child exits — only then may another start touch this
	// directory's dependencies.
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			r.mu.Lock()
			s := r.state
			r.mu.Unlock()
			if s != StateStarting {
				release()
				return
			}
			select {
			case <-r.done:
				release()
				return
			case <-ticker.C:
			}
		}
	}()

	// Non-blocking: the record is published as StateStarting, so return it
	// immediately instead of blocking the caller's request on readiness.
	// spawn's background probe (probeReady) flips the record to Ready or
	// Failed on its own; clients poll /preview/status to observe the
	// transition and /preview/logs for live setup progress. This decouples
	// a long first-run `bun install` from the HTTP request lifetime — an
	// intermediary (Fly edge, tunnel, mobile) that times out a minutes-long
	// held request can no longer cancel readiness and tear the install down
	// mid-write. A start that never comes up settles as StateFailed and is
	// evicted lazily by the next Start's stale-entry check above (its
	// gateway token expires on its own TTL).
	m.log.Printf("preview: starting %s on port %d for %s/%s", r.spec.Kind, r.port, worktreeID, serviceName)
	return r.snapshot(), nil
}

// Stop terminates every dev server registered under worktreeID. In v1
// that's the single "default" service; future multi-service callers
// can call once and have all services for the worktree torn down.
// Blocks until each process tree is reaped. Returns ErrNotRunning
// when no services exist for the worktree.
func (m *Manager) Stop(worktreeID string) error {
	if worktreeID == "" {
		return fmt.Errorf("preview: worktree id is required")
	}
	m.mu.Lock()
	victims := make([]*running, 0)
	victimKeys := make([]serviceKey, 0)
	for k, r := range m.servers {
		if k.WorktreeID == worktreeID {
			victims = append(victims, r)
			victimKeys = append(victimKeys, k)
		}
	}
	for _, k := range victimKeys {
		delete(m.servers, k)
	}
	m.mu.Unlock()

	if len(victims) == 0 {
		return ErrNotRunning
	}

	for i, r := range victims {
		r.stopWithGrace(m.stopGrace)
		m.revokeBestEffort(victimKeys[i].WorktreeID, victimKeys[i].ServiceName)
		m.log.Printf("preview: stopped %s/%s", victimKeys[i].WorktreeID, victimKeys[i].ServiceName)
	}
	return nil
}

// Status returns the snapshot for (worktreeID, "default") in v1.
// Future multi-service callers will get a Status-per-service API.
// Runs Detect every call when there's no running server so the
// Available bit reflects on-disk truth.
func (m *Manager) Status(_ context.Context, worktreeID, workDir string) (Status, error) {
	key := serviceKey{WorktreeID: worktreeID, ServiceName: defaultServiceName()}
	m.mu.Lock()
	r, ok := m.servers[key]
	m.mu.Unlock()
	if ok {
		r.touch()
		return r.snapshot(), nil
	}

	spec, err := Detect(workDir, m.packagerPolicy)
	if err != nil {
		return Status{}, fmt.Errorf("detect: %w", err)
	}
	out := Status{State: StateStopped}
	if spec != nil {
		out.Available = true
		out.Kind = spec.Kind
	}
	return out, nil
}

// LogTail returns the last N bytes of stdout/stderr captured from the
// "default" service for worktreeID. Returns nil when no server is
// running.
func (m *Manager) LogTail(worktreeID string) []byte {
	key := serviceKey{WorktreeID: worktreeID, ServiceName: defaultServiceName()}
	m.mu.Lock()
	r, ok := m.servers[key]
	m.mu.Unlock()
	if !ok || r.logs == nil {
		return nil
	}
	return r.logs.Snapshot()
}

// Shutdown stops every running server (and revokes its token) plus
// the reaper goroutine. Idempotent.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stopCh)
	live := m.servers
	m.servers = make(map[serviceKey]*running)
	m.mu.Unlock()

	for k, r := range live {
		r.stopWithGrace(m.stopGrace)
		m.revokeBestEffort(k.WorktreeID, k.ServiceName)
		m.log.Printf("preview: shutdown stopped %s/%s", k.WorktreeID, k.ServiceName)
	}
	m.reaperWG.Wait()
	// Cancel the manager-scoped context last so any straggler spawns
	// (e.g. an in-flight Start that lost the closed-check race) get
	// torn down through their CommandContext linkage.
	m.bgCancel()
}

// reaperLoop sweeps stale servers at reaperInterval. Exits on Shutdown.
func (m *Manager) reaperLoop() {
	defer m.reaperWG.Done()
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.reapIdle()
		}
	}
}

// reapIdle stops every server whose lastTouch is older than
// idleTimeout. Snapshots the candidates under m.mu, then runs the
// kill outside the lock so a long stopProcessGroup wait doesn't stall
// other Start/Stop calls.
//
// NB: lastTouch is bumped at spawn and on control-plane reads (Status,
// idempotent Start) — that's the CLI keepalive + phone polling. Actual
// preview traffic doesn't touch: LAN Metro traffic never crosses the
// daemon, and gateway-proxied requests terminate one hop earlier. A
// per-request touch via a gateway webhook is still TODO for the cloud
// path once the multi-service shape lands.
func (m *Manager) reapIdle() {
	cutoff := time.Now().Add(-m.idleTimeout)
	type victim struct {
		key serviceKey
		rec *running
	}
	var victims []victim
	m.mu.Lock()
	for k, r := range m.servers {
		r.mu.Lock()
		stale := r.lastTouch.Before(cutoff)
		r.mu.Unlock()
		if stale {
			victims = append(victims, victim{key: k, rec: r})
		}
	}
	for _, v := range victims {
		delete(m.servers, v.key)
	}
	m.mu.Unlock()

	for _, v := range victims {
		v.rec.stopWithGrace(m.stopGrace)
		m.revokeBestEffort(v.key.WorktreeID, v.key.ServiceName)
		m.log.Printf("preview: reaped idle %s/%s (idle > %s)", v.key.WorktreeID, v.key.ServiceName, m.idleTimeout)
	}
}

// workdirLease returns the bootstrap mutex for workDir, keyed by its
// canonical absolute path so the same folder arriving as a worktree
// ID, a folder slug, or through a symlinked prefix (macOS /tmp) maps
// to one lease. Leases are never removed — a handful of small mutexes
// per previewed folder for the process lifetime.
func (m *Manager) workdirLease(workDir string) *sync.Mutex {
	canon := workDir
	if abs, err := filepath.Abs(canon); err == nil {
		canon = abs
	}
	if resolved, err := filepath.EvalSymlinks(canon); err == nil {
		canon = resolved
	}
	m.bootMu.Lock()
	defer m.bootMu.Unlock()
	l, ok := m.bootLeases[canon]
	if !ok {
		l = &sync.Mutex{}
		m.bootLeases[canon] = l
	}
	return l
}

// revokeBestEffort calls the gateway's revoke webhook with a bounded
// timeout. Logs failures and moves on — the gateway's row will
// eventually expire if the call never lands.
func (m *Manager) revokeBestEffort(worktreeID, serviceName string) {
	ctx, cancel := context.WithTimeout(context.Background(), gwRevokeTimeout)
	defer cancel()
	if err := m.gw.Revoke(ctx, RevokeRequest{WorktreeID: worktreeID, ServiceName: serviceName}); err != nil {
		m.log.Printf("preview: gateway revoke for %s/%s failed (non-fatal): %v", worktreeID, serviceName, err)
	}
}

// defaultServiceName returns the "default" service name for v1.
// Kept in a tiny helper so the multi-service migration is a
// search-and-replace away.
func defaultServiceName() string {
	return "default"
}
