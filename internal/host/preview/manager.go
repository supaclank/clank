package preview

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

// Default lifecycle timers. Bumpable via Options for tests.
const (
	DefaultIdleTimeout = 15 * time.Minute
	DefaultStopGrace   = 3 * time.Second
	reaperInterval     = 1 * time.Minute
)

// ErrNotPreviewable is returned by Start when Detect found nothing to
// spawn. The mux handler maps it to a 404 with a structured code so
// the mobile UI can fall back to hiding the button.
var ErrNotPreviewable = errors.New("preview: worktree is not previewable")

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

	// Bump is invoked on every proxy request before the request is
	// forwarded. Wire to internal/keepalive Loop.Bump so previewing
	// counts as "user is active" and the sprite doesn't hibernate
	// mid-HMR. Nil disables the bump entirely (laptop dev mode).
	Bump func()

	// Log is the logger; nil falls back to the default logger.
	Log *log.Logger
}

// Manager owns the per-worktree dev-server registry and exposes the
// lifecycle (Start/Stop/Status) plus a reverse-proxy handler. One
// Manager lives on each host.Service.
type Manager struct {
	idleTimeout time.Duration
	stopGrace   time.Duration
	bump        func()
	log         *log.Logger

	mu       sync.Mutex
	servers  map[string]*running // keyed by worktree ID
	proxies  map[string]*httputil.ReverseProxy
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
		bump:        opts.Bump,
		log:         opts.Log,
		servers:     make(map[string]*running),
		proxies:     make(map[string]*httputil.ReverseProxy),
		stopCh:      make(chan struct{}),
		bgCtx:       bgCtx,
		bgCancel:    cancel,
	}
	m.reaperWG.Add(1)
	go m.reaperLoop()
	return m
}

// Start spawns the dev server for (worktreeID, workDir) and returns
// its Status. Idempotent — a second Start for the same worktree
// returns the existing server's snapshot without re-spawning.
//
// previewURLBase is the public URL Metro will bake into manifest URLs;
// see spawnRequest for the contract. Required.
//
// Returns ErrNotPreviewable when Detect found no recognizable framework.
func (m *Manager) Start(ctx context.Context, worktreeID, workDir, previewURLBase string) (Status, error) {
	spec, err := Detect(workDir)
	if err != nil {
		return Status{}, fmt.Errorf("detect: %w", err)
	}
	if spec == nil {
		return Status{}, ErrNotPreviewable
	}
	return m.startWithSpec(ctx, worktreeID, workDir, previewURLBase, *spec, 0)
}

// startWithSpec is the lock+spawn+register core that Start wraps.
// Split out so tests can drive it with a custom Spec (and per-test
// ReadyTimeout) without mutating package-level vars in parallel — the
// earlier global-mutation approach raced under -race.
//
// readyTimeout==0 falls through to the package default inside spawn.
func (m *Manager) startWithSpec(ctx context.Context, worktreeID, workDir, previewURLBase string, spec Spec, readyTimeoutOverride time.Duration) (Status, error) {
	if worktreeID == "" {
		return Status{}, fmt.Errorf("preview: worktree id is required")
	}
	if workDir == "" {
		return Status{}, fmt.Errorf("preview: work dir is required")
	}
	if previewURLBase == "" {
		return Status{}, fmt.Errorf("preview: preview_url_base is required")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Status{}, fmt.Errorf("preview: manager is shut down")
	}
	if existing, ok := m.servers[worktreeID]; ok {
		snap := existing.snapshot()
		m.mu.Unlock()
		return snap, nil
	}
	m.mu.Unlock()

	// Use the manager's background context, NOT the caller's. spawn
	// wires this to exec.CommandContext, and the caller's ctx (the
	// HTTP request context in production) gets canceled the moment
	// Start writes its response — that would SIGKILL Metro before it
	// printed a single line.
	r, err := spawn(m.bgCtx, spawnRequest{
		WorkDir:        workDir,
		Spec:           spec,
		PreviewURLBase: previewURLBase,
		ReadyTimeout:   readyTimeoutOverride,
	})
	if err != nil {
		return Status{}, fmt.Errorf("spawn: %w", err)
	}

	proxy, err := newReverseProxy(r.port, previewURLBase)
	if err != nil {
		// Tear down the orphaned child — we own it now and the caller
		// has no handle to stop it via the registry.
		r.stopWithGrace(m.stopGrace)
		return Status{}, fmt.Errorf("build proxy: %w", err)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		r.stopWithGrace(m.stopGrace)
		return Status{}, fmt.Errorf("preview: manager is shut down")
	}
	if existing, ok := m.servers[worktreeID]; ok {
		// Lost the race against a concurrent Start. Keep theirs, drop
		// ours — port leak only for the stop-grace window, never persistent.
		snap := existing.snapshot()
		m.mu.Unlock()
		r.stopWithGrace(m.stopGrace)
		m.log.Printf("preview: discarded duplicate spawn for worktree %s", worktreeID)
		return snap, nil
	}
	m.servers[worktreeID] = r
	m.proxies[worktreeID] = proxy
	m.mu.Unlock()
	m.log.Printf("preview: started %s on port %d for worktree %s", r.spec.Kind, r.port, worktreeID)
	return r.snapshot(), nil
}

// Stop terminates the dev server for worktreeID. Blocks until the
// process tree is reaped. Idempotent — returns ErrNotRunning when
// there's nothing to stop.
func (m *Manager) Stop(worktreeID string) error {
	m.mu.Lock()
	r, ok := m.servers[worktreeID]
	if !ok {
		m.mu.Unlock()
		return ErrNotRunning
	}
	delete(m.servers, worktreeID)
	delete(m.proxies, worktreeID)
	m.mu.Unlock()

	// Don't hold m.mu across the wait — it can block up to stopGrace.
	r.stopWithGrace(m.stopGrace)
	m.log.Printf("preview: stopped worktree %s", worktreeID)
	return nil
}

// Status returns the current state for worktreeID, freshly running
// Detect every call so the Available field reflects the on-disk truth
// (the user may have just deleted package.json).
//
// When no server is running, returns a {Available, State: Stopped}
// snapshot. When one is running, returns its real snapshot.
func (m *Manager) Status(_ context.Context, worktreeID, workDir string) (Status, error) {
	m.mu.Lock()
	r, ok := m.servers[worktreeID]
	m.mu.Unlock()
	if ok {
		return r.snapshot(), nil
	}

	spec, err := Detect(workDir)
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

// ProxyHandler returns an http.Handler that strips the given prefix
// from the request path and forwards to the running dev server for
// worktreeID. 404s when no server is running.
//
// Every successful dispatch bumps lastTouch (for the idle reaper) and
// calls the keepalive Bump callback (so an active preview holds the
// sprite awake).
//
// prefixToStrip is what comes before the dev server's path — e.g.
// "/worktrees/<wid>/preview/proxy". The handler trims this so Metro
// sees its own URL space starting at /.
func (m *Manager) ProxyHandler(worktreeID, prefixToStrip string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		m.mu.Lock()
		r, ok := m.servers[worktreeID]
		proxy := m.proxies[worktreeID]
		m.mu.Unlock()
		if !ok || proxy == nil {
			http.Error(w, "no preview server running for this worktree; call POST .../preview/start first", http.StatusNotFound)
			return
		}

		r.mu.Lock()
		r.lastTouch = time.Now()
		r.mu.Unlock()
		if m.bump != nil {
			m.bump()
		}

		// Strip the route prefix in place. Mirrors gateway/proxy.go's
		// singleJoiningSlash dance: leave a single leading slash so
		// the dev server sees "/" instead of "".
		if prefixToStrip != "" && strings.HasPrefix(req.URL.Path, prefixToStrip) {
			req2 := req.Clone(req.Context())
			req2.URL.Path = strings.TrimPrefix(req.URL.Path, prefixToStrip)
			if req2.URL.Path == "" || req2.URL.Path[0] != '/' {
				req2.URL.Path = "/" + req2.URL.Path
			}
			if req.URL.RawPath != "" {
				req2.URL.RawPath = strings.TrimPrefix(req.URL.RawPath, prefixToStrip)
				if req2.URL.RawPath == "" || req2.URL.RawPath[0] != '/' {
					req2.URL.RawPath = "/" + req2.URL.RawPath
				}
			}
			proxy.ServeHTTP(w, req2)
			return
		}
		proxy.ServeHTTP(w, req)
	})
}

// LogTail returns the last N bytes of stdout/stderr captured from the
// dev server, or nil if no server is running. Used by status endpoints
// and the mobile loading screen.
func (m *Manager) LogTail(worktreeID string) []byte {
	m.mu.Lock()
	r, ok := m.servers[worktreeID]
	m.mu.Unlock()
	if !ok || r.logs == nil {
		return nil
	}
	return r.logs.Snapshot()
}

// Shutdown stops every running server and the reaper goroutine.
// Idempotent.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stopCh)
	live := m.servers
	m.servers = make(map[string]*running)
	m.proxies = make(map[string]*httputil.ReverseProxy)
	m.mu.Unlock()

	for wid, r := range live {
		r.stopWithGrace(m.stopGrace)
		m.log.Printf("preview: shutdown stopped worktree %s", wid)
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
func (m *Manager) reapIdle() {
	cutoff := time.Now().Add(-m.idleTimeout)
	type victim struct {
		id   string
		rec  *running
	}
	var victims []victim
	m.mu.Lock()
	for wid, r := range m.servers {
		r.mu.Lock()
		stale := r.lastTouch.Before(cutoff)
		r.mu.Unlock()
		if stale {
			victims = append(victims, victim{id: wid, rec: r})
		}
	}
	for _, v := range victims {
		delete(m.servers, v.id)
		delete(m.proxies, v.id)
	}
	m.mu.Unlock()

	for _, v := range victims {
		v.rec.stopWithGrace(m.stopGrace)
		m.log.Printf("preview: reaped idle worktree %s (idle > %s)", v.id, m.idleTimeout)
	}
}
