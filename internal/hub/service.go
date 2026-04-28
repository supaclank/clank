// This file contains the core Service struct, constructor, and HTTP
// serve loop for the Hub control plane. The host-catalog primitives
// are in hub.go; topical method sets are in sessions.go, events.go,
// permissions.go, voice.go, etc.
//
// Service does NOT touch the filesystem for its own listener or PID
// file — those are caller responsibilities (daemoncli in production,
// startHubOnSocket in tests). Run takes a pre-bound net.Listener and
// returns when Stop() is called or s.ctx is otherwise cancelled.
package hub

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostclient "github.com/acksell/clank/internal/host/client"
	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/internal/voice"
	"github.com/coder/websocket"
)

// Service is the Hub control plane.
//
// It manages coding agent sessions (OpenCode, Claude Code) — currently
// in-process or via a single clank-host subprocess — aggregates their
// events, and exposes an HTTP API to its caller-supplied net.Listener
// (a Unix domain socket in production).
type Service struct {
	// hosts is the catalog of registered Host endpoints, keyed by
	// Hostname. Phase 2 only uses a single "local" host. Multi-host
	// dispatch arrives with the TCP+TLS transport in Phase 4. All
	// session-targeting code paths resolve through s.hostFor(hostname)
	// against this map; there is no shortcut field.
	hostsMu sync.RWMutex
	hosts   map[host.Hostname]*hostclient.HTTP

	mu       sync.RWMutex
	sessions map[string]*managedSession // keyed by hub session ID
	// subscribers receive all events broadcast by the hub.
	subMu       sync.RWMutex
	subscribers map[string]chan agent.Event // keyed by subscriber ID

	startTime time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	// stopCh is the dedicated "please stop Run()" signal. It is closed
	// by Stop() (and by the Serve-error goroutine on a fatal listener
	// failure) so Run can begin shutdown WITHOUT cancelling s.ctx
	// first. Cancelling s.ctx pre-drain would abort in-flight handlers
	// that capture it, defeating the 5s graceful-shutdown window.
	// s.cancel() is invoked at the very end of shutdown(), after
	// server.Shutdown returns, so callers using s.ctx for long-lived
	// background work still see termination.
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// startupDiscoverDone is closed when runStartupDiscover has finished
	// its single startup pass. Tests (and any caller that needs a
	// deterministic post-boot state) wait on it via WaitStartupDiscover.
	// Created fresh in New(); closed exactly once by the goroutine that
	// runs the discover pass in Run().
	startupDiscoverDone chan struct{}

	// BackendManagers maps each backend type to its manager. The manager
	// creates SessionBackend instances and owns shared resources (e.g.,
	// OpenCode servers). Managers that also implement agent.AgentLister or
	// agent.SessionDiscoverer gain agent listing and session discovery
	// capabilities automatically.
	//
	// As of Phase 1, this field is **only used by tests**. Production
	// clankd injects a HostClient via SetHostClient (the clank-host
	// subprocess owns the real BackendManagers). Tests still use the
	// `s.BackendManagers[X] = mgr` pattern; when Run() finds no host
	// registered in the catalog it builds an in-process host from this map.
	//
	// Removed in Phase 2 once tests get a `WithHost` constructor option.
	BackendManagers map[agent.BackendType]agent.BackendManager

	// Store is the optional SQLite persistence layer. When non-nil, session
	// metadata is written through on every mutation and loaded on startup.
	// When nil (e.g. in tests), the daemon operates purely in-memory.
	Store *store.Store

	// launchers holds HostLaunchers keyed by provider name. Populated by
	// SetHostLauncher; consulted in createSession when a request carries
	// LaunchHost. Empty in laptop-hub mode (no provisioning needed there).
	//
	// defaultLaunchHostSpec (if non-nil) is applied to StartRequests
	// that omit LaunchHost — used by cloud hubs so TUI-created sessions
	// get auto-launched in a sandbox without each client needing to
	// know about launchers. Reads/writes are guarded by launchersMu.
	launchersMu           sync.RWMutex
	launchers             map[string]HostLauncher
	defaultLaunchHostSpec *agent.LaunchHostSpec

	// primaryAgentsRefreshMu guards primaryAgentsRefreshInFlight.
	primaryAgentsRefreshMu       sync.Mutex
	primaryAgentsRefreshInFlight map[string]bool // keyed by catalogKey (backend, hostname, gitRef)

	// voice is the singleton voice session (nil when inactive).
	voice          *voice.Session
	voiceAudioConn *websocket.Conn

	log *log.Logger
}

// managedSession tracks a running agent session.
type managedSession struct {
	info         agent.SessionInfo
	backend      agent.SessionBackend   // nil until started
	watchOnly    bool                   // true when backend was started via Watch() (no prompt sent yet)
	pendingPerms []agent.PermissionData // queue of permission prompts awaiting responses
}

// New constructs an in-memory Hub Service. It does not touch the
// filesystem or open any sockets — wire up BackendManagers / Store /
// HostClient as needed, then hand the result to a Run-driver
// (daemoncli.startServer in production, startHubOnSocket in tests).
func New() *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		hosts:                        make(map[host.Hostname]*hostclient.HTTP),
		sessions:                     make(map[string]*managedSession),
		subscribers:                  make(map[string]chan agent.Event),
		startTime:                    time.Now(),
		ctx:                          ctx,
		cancel:                       cancel,
		stopCh:                       make(chan struct{}),
		startupDiscoverDone:          make(chan struct{}),
		log:                          log.New(os.Stderr, "[clank-hub] ", log.LstdFlags|log.Lmsgprefix),
		BackendManagers:              make(map[agent.BackendType]agent.BackendManager),
		primaryAgentsRefreshInFlight: make(map[string]bool),
	}
}

// Run serves the Hub HTTP API on the provided listener and blocks
// until Stop() is called (or s.ctx is cancelled some other way). The
// caller owns the listener's lifetime AND any on-disk artifacts (PID
// file, socket file, etc.); Run never touches them.
//
// handler is the HTTP handler the listener should serve. Production
// uses internal/hub/mux (hubmux.New(s, log).Handler()); tests use the
// same. Run takes the handler from the caller (rather than wiring it
// internally) to keep the hub package free of an import cycle on
// internal/hub/mux, which itself imports internal/hub.
func (s *Service) Run(listener net.Listener, handler http.Handler) error {
	s.log.Printf("hub started (pid=%d, addr=%s)", os.Getpid(), listener.Addr())

	// Load persisted sessions from the store (if available).
	if s.Store != nil {
		sessions, err := s.Store.LoadSessions()
		if err != nil {
			s.log.Printf("warning: failed to load sessions from store: %v", err)
		} else {
			s.mu.Lock()
			for _, info := range sessions {
				// Sessions loaded from DB have no backend — they can't
				// be actively busy. Normalize stale statuses to idle so
				// the inbox doesn't show spinners or dead markers for
				// sessions interrupted by a daemon restart.
				if info.Status == agent.StatusBusy || info.Status == agent.StatusStarting || info.Status == agent.StatusDead {
					info.Status = agent.StatusIdle
				}
				s.sessions[info.ID] = &managedSession{info: info, backend: nil}
			}
			s.mu.Unlock()
			if len(sessions) > 0 {
				s.log.Printf("loaded %d sessions from store", len(sessions))
			}
		}
	}

	// Per Decision #3 (no in-process Host shortcut), the caller MUST
	// register at least the "local" host before Run(). Production clankd
	// does this via SetHostClient(*hostclient.HTTP) pointed at the
	// clank-host subprocess; tests do it through the test helper which
	// spins a real host.Service behind an httptest server.
	if _, err := s.hostFor("local"); err != nil {
		return fmt.Errorf("hub.Service.Run: no host client registered (call SetHostClient before Run)")
	}

	// Warm primary agent caches AFTER the reconciler has started. The
	// cache warmer uses GetOrStartServer which will wait for the reconciler
	// to finish starting servers rather than starting them itself.
	s.warmPrimaryAgentCaches()

	// Backfill GitRef.Local for any sessions whose path metadata was
	// lost. Runs in a background goroutine so listener readiness isn't
	// blocked by an OpenCode server boot.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runStartupDiscover(s.ctx)
		close(s.startupDiscoverDone)
	}()

	server := &http.Server{Handler: handler}

	// Start HTTP server in background. Surface a real Serve error
	// (anything other than the normal "we asked it to close") via a
	// channel so Run can return it instead of silently shutting down.
	serveErrCh := make(chan error, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			s.log.Printf("http serve error: %v", err)
			serveErrCh <- err
			// Trigger shutdown so Run unblocks promptly on a
			// listener-level failure rather than waiting for an
			// external cancellation. Use stopCh (not s.cancel)
			// because shutdown() needs s.ctx alive during the
			// HTTP drain.
			s.triggerStop()
			return
		}
		serveErrCh <- nil
	}()

	// Wait for shutdown (Stop(), external ctx cancellation, or a
	// fatal Serve error above). Selecting on both stopCh and
	// s.ctx.Done() preserves the legacy "external context cancelled"
	// path used by tests that wire ctx for cleanup.
	select {
	case <-s.stopCh:
	case <-s.ctx.Done():
	}
	s.log.Printf("context cancelled, shutting down")

	shutdownErr := s.shutdown(server)
	// Prefer the underlying Serve error when present — it's the
	// proximate cause that callers want to react to.
	if serveErr := <-serveErrCh; serveErr != nil {
		return serveErr
	}
	return shutdownErr
}

// shutdown gracefully tears down internal subsystems and the HTTP
// server. The on-disk listener artifacts (socket file, PID file) are
// the caller's responsibility — Run never created them.
//
// Order matters: drain HTTP first so in-flight handlers can complete
// (they capture s.ctx and would abort early if we cancelled it up
// front), then tear down the dependencies they touch, then cancel
// s.ctx as the very last step to terminate any long-lived background
// work that survives the drain.
func (s *Service) shutdown(server *http.Server) error {
	// Drain HTTP first: stop accepting new connections and let
	// in-flight handlers complete before we tear down the dependencies
	// (store, host clients, subscribers) they may still be touching.
	// Otherwise active requests can hit closed channels or a closed
	// store and return confusing errors to clients that should have
	// either succeeded or seen a clean shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		s.log.Printf("http shutdown error: %v", err)
	}

	// Stop the voice session if active.
	s.mu.Lock()
	if s.voice != nil {
		s.log.Println("stopping voice session")
		s.voice.Close()
		s.voice = nil
	}
	if s.voiceAudioConn != nil {
		s.voiceAudioConn.CloseNow()
		s.voiceAudioConn = nil
	}
	s.mu.Unlock()

	// Release every active session backend. Stop() unblocks the
	// per-session SSE relay goroutines (started by runBackend) by
	// instructing the host to close their event streams; without this
	// step s.wg.Wait below would deadlock, since we no longer own the
	// host plane and thus can't shut it down to cascade-close streams.
	//
	// In production, this is also what TestDaemonGracefulShutdownStopsBackends
	// pins down: shutting hub down must release the agent processes it
	// asked the host to spawn, even when the host process outlives hub.
	s.stopActiveBackends()

	// Disconnect every host client in the catalog. The host plane's
	// lifetime (process or test fixture) is owned by the caller; we
	// only release our HTTP transport handles here.
	s.closeHosts()

	// Close the persistence store.
	if s.Store != nil {
		if err := s.Store.Close(); err != nil {
			s.log.Printf("error closing store: %v", err)
		}
	}

	// Close all subscriber channels.
	s.subMu.Lock()
	for id, ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, id)
	}
	s.subMu.Unlock()

	s.wg.Wait()
	// Cancel s.ctx now that the HTTP drain and dependent shutdowns
	// have finished. Any background goroutine still selecting on
	// s.ctx.Done() (e.g. runStartupDiscover) will exit; in-flight
	// handlers that captured s.ctx have already returned because
	// server.Shutdown above waited for them.
	s.cancel()
	s.log.Printf("hub stopped")
	return nil
}

// SetLogOutput redirects the daemon's logger to the given writer.
// Call before Run() to capture all daemon output.
func (s *Service) SetLogOutput(w io.Writer) {
	s.log.SetOutput(w)
}

// SetHostClient injects the host client. Call before Run(). The caller
// owns the host plane's lifetime (e.g. clankd's host supervisor for the
// production clank-host subprocess, or the test helper for in-test
// httptest fixtures).
//
// Equivalent to RegisterHost("local", c); kept as a convenience for the
// existing call sites that predate the host catalog.
func (s *Service) SetHostClient(c *hostclient.HTTP) {
	_, _ = s.RegisterHost("local", c)
}

// Stop requests the daemon to shut down. Safe to call from any
// goroutine; idempotent.
func (s *Service) Stop() {
	s.triggerStop()
}

// WaitStartupDiscover blocks until the post-Run() startup-discover pass
// (which heals mis-tagged info.Backend rows and backfills GitRef.LocalPath)
// has completed, or until ctx is cancelled. Returns ctx.Err() on timeout.
//
// Used by tests that exercise the post-restart "reopen a corrupted Claude
// session" code path: without this barrier, a caller can race the heal and
// hit the pre-fix bug (dispatch to the wrong backend manager → hang).
//
// In production, the daemon does not call this — handlers tolerate the
// brief window because (a) the inbox's user-triggered Discover also heals,
// and (b) most user clicks land after the goroutine completes given how
// fast Claude's file-IO discovery is.
func (s *Service) WaitStartupDiscover(ctx context.Context) error {
	select {
	case <-s.startupDiscoverDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// triggerStop closes stopCh exactly once. Used by Stop() and by the
// Serve-error path so Run unblocks without cancelling s.ctx.
func (s *Service) triggerStop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

// closeHosts disconnects every host client in the catalog. Errors are
// logged but not returned: shutdown is best-effort, and the caller
// (typically the Run-cleanup path) has nothing useful to do with a
// per-host close failure.
//
// Iterates a snapshot so we don't hold hostsMu across an external Close
// call. Safe to call multiple times — Close on an already-closed client
// is a no-op for both InProcess and HTTP transports.
func (s *Service) closeHosts() {
	for id, c := range s.snapshotHosts() {
		if err := c.Close(); err != nil {
			s.log.Printf("close host %s: %v", id, err)
		}
	}
}

// stopActiveBackends instructs every live SessionBackend to stop. This
// is the hub-side complement to host.Service.Shutdown: it releases the
// session resources hub asked the host to allocate, in the order it
// allocated them, without requiring host-process teardown to do it.
//
// Captures stable (sessionID, backend) pairs while holding s.mu so a
// concurrent activateBackend that mutates ms.backend cannot make us
// stop the wrong handle (or skip a backend whose pointer was nil at
// snapshot time but became non-nil before we got around to checking).
func (s *Service) stopActiveBackends() {
	type liveBackend struct {
		id      string
		backend agent.SessionBackend
	}
	s.mu.RLock()
	live := make([]liveBackend, 0, len(s.sessions))
	for _, ms := range s.sessions {
		if ms.backend != nil {
			live = append(live, liveBackend{id: ms.info.ID, backend: ms.backend})
		}
	}
	s.mu.RUnlock()

	for _, item := range live {
		if err := item.backend.Stop(); err != nil {
			s.log.Printf("stop session %s: %v", item.id, err)
		}
	}
}
