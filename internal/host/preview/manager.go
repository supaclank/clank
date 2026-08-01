package preview

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/acksell/clank/pkg/preview/tokens"
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

// ErrNotRunning is returned by Stop when no server exists for the
// worktree. Mapped to 404.
var ErrNotRunning = errors.New("preview: no preview server running for worktree")

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
}

// serviceKey is the registry key. Configured previews use their declared
// names; Expo retains the default service name.
type serviceKey struct {
	WorktreeID  string
	ServiceName string
}

// Manager owns the per-(worktree, service) dev-server registry and
// exposes the lifecycle (Start/Stop/Status). One Manager lives on
// each host.Service.
type Manager struct {
	idleTimeout time.Duration
	stopGrace   time.Duration
	log         *log.Logger
	gw          *GWClient

	mu       sync.Mutex
	servers  map[serviceKey]*running
	closed   bool
	reaperWG sync.WaitGroup
	stopCh   chan struct{}

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
		idleTimeout: opts.IdleTimeout,
		stopGrace:   opts.StopGrace,
		log:         opts.Log,
		gw:          opts.GWClient,
		servers:     make(map[serviceKey]*running),
		stopCh:      make(chan struct{}),
		bgCtx:       bgCtx,
		bgCancel:    cancel,
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
// Returns ErrSetupRequired when a non-Expo project has no launch config.
func (m *Manager) Start(ctx context.Context, worktreeID, workDir, launchName string) (Status, error) {
	if launchName != "" {
		key := serviceKey{WorktreeID: worktreeID, ServiceName: launchName}
		m.mu.Lock()
		r, ok := m.servers[key]
		if ok {
			snapshot := r.snapshot()
			if snapshot.State == StateReady || snapshot.State == StateStarting {
				m.mu.Unlock()
				r.touch()
				return snapshot, nil
			}
			delete(m.servers, key)
		}
		m.mu.Unlock()
	}
	launch, err := resolveLaunch(workDir, launchName)
	if err != nil {
		return Status{}, fmt.Errorf("resolve launch: %w", err)
	}
	return m.startWithSpec(ctx, worktreeID, launch.WorkDir, launch.ServiceName, launch.Spec, 0)
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

	// Allocate the listen port BEFORE registering so the gateway can
	// reserve a route to it AND we can thread the resulting public URL
	// into Metro's EXPO_PACKAGER_PROXY_URL. Order matters: Metro reads
	// the env var at startup, so we have to know the URL before spawn.
	port, err := allocatePort()
	if err != nil {
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
	// hides the native redbox). Expo-only.
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
	// printed a single line.
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
		if regErr == nil {
			// TODO(ai-review): revoke here uses (worktreeID, serviceName) which may evict the winner's live route if the gateway resolves by service pair rather than token. https://github.com/Acksell/clank/pull/36#discussion_r3324139755
			m.revokeBestEffort(worktreeID, serviceName)
		}
		m.log.Printf("preview: discarded duplicate spawn for %s/%s", worktreeID, serviceName)
		return snap, nil
	}
	m.servers[key] = r
	m.mu.Unlock()

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

// Stop terminates every dev server registered under worktreeID.
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

// StopService terminates one named preview without affecting sibling services.
func (m *Manager) StopService(worktreeID, serviceName string) error {
	if worktreeID == "" {
		return fmt.Errorf("preview: worktree id is required")
	}
	if serviceName == "" {
		return fmt.Errorf("preview: service name is required")
	}
	key := serviceKey{WorktreeID: worktreeID, ServiceName: serviceName}
	m.mu.Lock()
	r, ok := m.servers[key]
	if ok {
		delete(m.servers, key)
	}
	m.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}

	r.stopWithGrace(m.stopGrace)
	m.revokeBestEffort(worktreeID, serviceName)
	m.log.Printf("preview: stopped %s/%s", worktreeID, serviceName)
	return nil
}

// Status resolves Expo or the configured default launch.
func (m *Manager) Status(ctx context.Context, worktreeID, workDir string) (Status, error) {
	return m.StatusNamed(ctx, worktreeID, workDir, "")
}

// StatusNamed returns one named launch or its current running service.
func (m *Manager) StatusNamed(_ context.Context, worktreeID, workDir, launchName string) (Status, error) {
	if launchName != "" {
		key := serviceKey{WorktreeID: worktreeID, ServiceName: launchName}
		m.mu.Lock()
		r, ok := m.servers[key]
		m.mu.Unlock()
		if ok {
			r.touch()
			return r.snapshot(), nil
		}
	}
	launch, err := resolveLaunch(workDir, launchName)
	if err != nil {
		var setup *SetupRequiredError
		if errors.As(err, &setup) {
			return Status{
				State:             StateStopped,
				SetupRequired:     true,
				SetupPrompt:       setup.Prompt,
				ProjectConfigPath: setup.ProjectConfigPath,
				HostConfigPath:    setup.HostConfigPath,
			}, nil
		}
		return Status{}, fmt.Errorf("resolve launch: %w", err)
	}

	key := serviceKey{WorktreeID: worktreeID, ServiceName: launch.ServiceName}
	m.mu.Lock()
	r, ok := m.servers[key]
	m.mu.Unlock()
	if ok {
		r.touch()
		return r.snapshot(), nil
	}

	return Status{
		Available:   true,
		Kind:        launch.Spec.Kind,
		ServiceName: launch.ServiceName,
		State:       StateStopped,
	}, nil
}

// LogTail returns the default Expo service's stdout/stderr tail.
func (m *Manager) LogTail(worktreeID string) []byte {
	return m.LogTailNamed(worktreeID, tokens.DefaultServiceName)
}

// LogTailNamed returns one named service's stdout/stderr tail.
func (m *Manager) LogTailNamed(worktreeID, serviceName string) []byte {
	key := serviceKey{WorktreeID: worktreeID, ServiceName: serviceName}
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
