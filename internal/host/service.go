package host

// Service is the Host plane's domain object: it owns BackendManagers
// for agent sessions and resolves GitRefs to working directories
// (local refs use the path directly; remote refs clone-on-first-use
// into <ClonesDir>/<CloneDirName(remote)>/).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host/store"
)

// Service is the Host plane's domain object. Construct with New; call
// Init to start background goroutines and Shutdown to release them.
// Owns a registry of live SessionBackends keyed by session ULID.
type Service struct {
	id              Hostname
	startedAt       time.Time
	backendManagers map[agent.BackendType]agent.BackendManager
	auth            *AuthManager
	log             *log.Logger

	mu       sync.RWMutex
	sessions map[string]agent.SessionBackend
	// closed gates new registrations. CreateSession re-checks after
	// the slow CreateBackend call to avoid leaking into a torn-down
	// registry.
	closed bool

	// clonesDir parents per-remote clones at <clonesDir>/<CloneDirName>/.
	// Defaults to ~/.clank/clones; tests override via Options.
	clonesDir string

	// cloneSF deduplicates concurrent first-use clones of the same
	// remote URL.
	cloneSF singleflight.Group

	// branches caches listBranches per projectDir. Without it, the
	// inbox poll fans out to ~4 git subprocesses per worktree every
	// 3s.
	branches *branchCache

	// sessionsStore persists session metadata. Nil in tests that don't
	// need persistence; production wiring always provides one.
	sessionsStore *store.Store

	// subscribers fans out backend events to SSE/WebSocket handlers.
	subscribers *subscriberRegistry

	// wg tracks per-session event-relay goroutines so Shutdown can
	// wait for them before closing the subscriber registry.
	wg sync.WaitGroup
}

// Options configures a Service at construction time.
type Options struct {
	// ID is the host identifier. Defaults to HostLocal when empty.
	ID Hostname
	// BackendManagers maps each backend type to its manager. Required.
	BackendManagers map[agent.BackendType]agent.BackendManager
	// Log is the logger. Defaults to a logger writing to stderr with the
	// "[clank-host]" prefix.
	Log *log.Logger
	// ClonesDir is the parent directory under which workDirFor clones
	// remote refs on first use. Defaults to ~/.clank/clones when empty.
	// Tests should set this to a t.TempDir().
	ClonesDir string
	// BranchCacheTTL overrides the default TTL for the listBranches
	// cache. Zero uses DefaultBranchCacheTTL. Tests set this to control
	// staleness behavior.
	BranchCacheTTL time.Duration
	// Now overrides the clock used by the listBranches cache. Tests
	// inject a controllable clock to assert cache hit/miss behavior
	// without sleeping. Nil means time.Now.
	Now func() time.Time

	// SessionsStore persists session metadata. Required in production;
	// optional in tests. When nil, session-metadata methods return
	// SessionStoreNotConfigured.
	SessionsStore *store.Store
}

// New creates a Service. Panics on missing BackendManagers — fast
// failure beats a later nil deref.
func New(opts Options) *Service {
	if opts.BackendManagers == nil {
		panic("host.New: BackendManagers is required")
	}
	id := opts.ID
	if id == "" {
		id = HostLocal
	}
	lg := opts.Log
	if lg == nil {
		lg = log.New(os.Stderr, "[clank-host] ", log.LstdFlags|log.Lmsgprefix)
	}
	s := &Service{
		id:              id,
		startedAt:       time.Now(),
		backendManagers: opts.BackendManagers,
		log:             lg,
		sessions:        make(map[string]agent.SessionBackend),
		clonesDir:       opts.ClonesDir,
		branches:        newBranchCache(opts.BranchCacheTTL, opts.Now),
		sessionsStore:   opts.SessionsStore,
		subscribers:     newSubscriberRegistry(),
	}
	if s.clonesDir == "" {
		// On home-dir lookup failure leave the field empty —
		// workDirFor will reject remote refs loudly instead of guessing.
		if home, err := os.UserHomeDir(); err == nil {
			s.clonesDir = filepath.Join(home, ".clank", "clones")
		}
	}

	// Auth manager is opencode-only: it tells the OpenCode backend to
	// restart its servers when auth.json changes. Skip silently when
	// the backend isn't registered.
	if oc, ok := s.backendManagers[agent.BackendOpenCode].(*OpenCodeBackendManager); ok {
		am, err := NewAuthManager(func(ctx context.Context) error {
			return oc.ServerManager().RestartAllServers(ctx)
		})
		if err != nil {
			s.log.Printf("auth manager unavailable: %v", err)
		} else {
			s.auth = am
		}
	} else {
		s.log.Printf("auth manager unavailable: opencode backend not registered")
	}

	return s
}

// Auth returns the AuthManager, or nil when the OpenCode backend
// isn't registered. Callers must nil-check.
func (s *Service) Auth() *AuthManager { return s.auth }

// ID returns the host's ID.
func (s *Service) ID() Hostname { return s.id }

// Init initializes all BackendManagers. knownDirs returns previously-
// seen project directories per backend (used to warm long-lived
// servers like OpenCode); pass a func returning nil to skip warm-up.
// Non-blocking — managers run reconciler goroutines for the lifetime
// of ctx.
func (s *Service) Init(ctx context.Context, knownDirs func(agent.BackendType) ([]string, error)) error {
	// Normalize stale runtime statuses from the previous daemon run —
	// busy/starting sessions have no live backend now, and without
	// this sweep the inbox would show them as forever-spinners.
	s.normalizeStaleSessionStatus(ctx)

	for bt, mgr := range s.backendManagers {
		bt := bt
		fn := func() ([]string, error) {
			if knownDirs == nil {
				return nil, nil
			}
			return knownDirs(bt)
		}
		if err := mgr.Init(ctx, fn); err != nil {
			s.log.Printf("warning: init %s backend: %v", bt, err)
		}
	}
	return nil
}

// normalizeStaleSessionStatus rewrites busy/starting/dead sessions
// (states that require a live backend to advance) back to idle.
// idle/error are stable enough to leave alone.
func (s *Service) normalizeStaleSessionStatus(ctx context.Context) {
	if s.sessionsStore == nil {
		return
	}
	sessions, err := s.sessionsStore.ListSessions(ctx)
	if err != nil {
		s.log.Printf("warning: list sessions for status sweep: %v", err)
		return
	}
	var fixed int
	for _, info := range sessions {
		switch info.Status {
		case agent.StatusBusy, agent.StatusStarting, agent.StatusDead:
			info.Status = agent.StatusIdle
			// Don't bump UpdatedAt — a cleanup shouldn't hoist every
			// recovered session to the top of the inbox.
			if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
				s.log.Printf("warning: normalize status for %s: %v", info.ID, err)
				continue
			}
			fixed++
		}
	}
	if fixed > 0 {
		s.log.Printf("normalized %d stale session status(es) to idle", fixed)
	}
}

// Shutdown stops live backends and then BackendManagers. Idempotent.
// Order: mark closed → stop backends (closes Events() → relays exit)
// → wait for relays → close subscribers → shut down managers.
func (s *Service) Shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	live := s.sessions
	s.sessions = make(map[string]agent.SessionBackend)
	s.mu.Unlock()
	for id, b := range live {
		if err := b.Stop(); err != nil {
			s.log.Printf("warning: stop session %s: %v", id, err)
		}
	}
	// Wait for relays before closing subscribers (a Broadcast in
	// flight against a closed registry would race). Bounded — a
	// misbehaving backend mustn't hang shutdown forever.
	relayDone := make(chan struct{})
	go func() { s.wg.Wait(); close(relayDone) }()
	select {
	case <-relayDone:
	case <-time.After(2 * time.Second):
		s.log.Printf("warning: event-relay goroutines did not drain within 2s; continuing shutdown")
	}
	if s.subscribers != nil {
		s.subscribers.CloseAll()
	}
	for bt, mgr := range s.backendManagers {
		s.log.Printf("shutting down %s backend manager", bt)
		mgr.Shutdown()
	}
}

// Status returns the current host status.
func (s *Service) Status(_ context.Context) (HostStatus, error) {
	s.mu.RLock()
	live := len(s.sessions)
	s.mu.RUnlock()
	return HostStatus{
		Hostname:  s.id,
		Version:   "", // Populated once we have a version string wired up
		StartedAt: s.startedAt,
		Sessions:  live,
	}, nil
}

// ListBackends returns the set of backends known to this host.
func (s *Service) ListBackends(_ context.Context) ([]BackendInfo, error) {
	out := make([]BackendInfo, 0, len(s.backendManagers))
	for bt := range s.backendManagers {
		out = append(out, BackendInfo{
			Name:        bt,
			DisplayName: string(bt),
			Available:   true,
		})
	}
	return out, nil
}

// ListAgents returns the agents the backend supports for ref's repo.
// (nil, nil) means the backend is unknown or doesn't implement listing
// — neither is an error.
func (s *Service) ListAgents(ctx context.Context, bt agent.BackendType, ref agent.GitRef) ([]AgentInfo, error) {
	mgr, ok := s.backendManagers[bt]
	if !ok {
		return nil, nil
	}
	lister, ok := mgr.(agent.AgentLister)
	if !ok {
		return nil, nil
	}
	workDir, err := s.workDirFor(ctx, ref)
	if err != nil {
		return nil, err
	}
	return lister.ListAgents(ctx, workDir)
}

// ListModels mirrors ListAgents for model catalogs.
func (s *Service) ListModels(ctx context.Context, bt agent.BackendType, ref agent.GitRef) ([]ModelInfo, error) {
	mgr, ok := s.backendManagers[bt]
	if !ok {
		return nil, nil
	}
	lister, ok := mgr.(agent.ModelLister)
	if !ok {
		return nil, nil
	}
	workDir, err := s.workDirFor(ctx, ref)
	if err != nil {
		return nil, err
	}
	return lister.ListModels(ctx, workDir)
}

// DiscoverSessions asks the backend manager for historical sessions.
// seedDir=="" hits AllSessionDiscoverer if implemented (global heal);
// otherwise SessionDiscoverer(seedDir). nil, nil for managers that
// implement neither.
func (s *Service) DiscoverSessions(ctx context.Context, bt agent.BackendType, seedDir string) ([]agent.SessionSnapshot, error) {
	mgr, ok := s.backendManagers[bt]
	if !ok {
		return nil, nil
	}
	if seedDir == "" {
		if all, ok := mgr.(agent.AllSessionDiscoverer); ok {
			snaps, err := all.DiscoverAllSessions(ctx)
			return s.persistSnapshots(ctx, snaps, err)
		}
	}
	disc, ok := mgr.(agent.SessionDiscoverer)
	if !ok {
		return nil, nil
	}
	snaps, err := disc.DiscoverSessions(ctx, seedDir)
	return s.persistSnapshots(ctx, snaps, err)
}

// persistSnapshots upserts newly-discovered session snapshots into the store,
// skipping any that already have a matching ExternalID. Returns all snapshots
// (including pre-existing ones) so callers can report totals.
func (s *Service) persistSnapshots(ctx context.Context, snaps []agent.SessionSnapshot, err error) ([]agent.SessionSnapshot, error) {
	if err != nil || s.sessionsStore == nil {
		return snaps, err
	}
	for _, snap := range snaps {
		if snap.ID == "" {
			continue
		}
		_, lookupErr := s.sessionsStore.FindSessionByExternalID(ctx, snap.ID)
		if lookupErr == nil {
			// Already registered — skip.
			continue
		}
		if !errors.Is(lookupErr, store.ErrSessionNotFound) {
			// Treat any non-NotFound store error as a real failure: skip
			// this snapshot rather than fall through to UpsertSession,
			// which could otherwise compound a lookup failure into an
			// unrelated insert.
			s.log.Printf("discover: lookup snapshot extID=%s: %v", snap.ID, lookupErr)
			continue
		}
		info := agent.SessionInfo{
			ID:              ulid.Make().String(),
			ExternalID:      snap.ID,
			Backend:         snap.Backend,
			Status:          agent.StatusIdle,
			Title:           snap.Title,
			RevertMessageID: snap.RevertMessageID,
			GitRef:          agent.GitRef{LocalPath: snap.Directory},
			CreatedAt:       snap.CreatedAt,
			UpdatedAt:       snap.UpdatedAt,
		}
		if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
			s.log.Printf("discover: persist snapshot extID=%s: %v", snap.ID, err)
		}
	}
	return snaps, nil
}

// CreateSession registers a fresh SessionBackend under sessionID. The
// backend is NOT started — callers call Start() or Watch().
//
// Returns the resolved serverURL (empty for backends without an HTTP
// server, e.g. Claude Code). req.GitRef is resolved to workDir via
// workDirFor.
func (s *Service) CreateSession(ctx context.Context, sessionID string, req agent.StartRequest) (agent.SessionBackend, string, error) {
	if sessionID == "" {
		return nil, "", fmt.Errorf("session id is required")
	}
	if req.Backend == "" {
		return nil, "", fmt.Errorf("backend is required")
	}
	mgr, ok := s.backendManagers[req.Backend]
	if !ok {
		return nil, "", fmt.Errorf("no backend manager for %s", req.Backend)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, "", fmt.Errorf("host service is shut down")
	}
	if _, exists := s.sessions[sessionID]; exists {
		s.mu.Unlock()
		return nil, "", fmt.Errorf("session %s already registered", sessionID)
	}
	s.mu.Unlock()

	if err := req.GitRef.Validate(); err != nil {
		return nil, "", fmt.Errorf("git_ref: %w", err)
	}
	workDir, err := s.workDirFor(ctx, req.GitRef)
	if err != nil {
		return nil, "", err
	}

	b, err := mgr.CreateBackend(ctx, agent.BackendInvocation{
		WorkDir:          workDir,
		ResumeExternalID: req.SessionID,
	})
	if err != nil {
		return nil, "", err
	}
	// Re-check closed and duplicate-id under the lock — CreateBackend
	// can take seconds, so a Shutdown or racing CreateSession could
	// have run in between.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if stopErr := b.Stop(); stopErr != nil {
			s.log.Printf("warning: stop backend created during shutdown: %v", stopErr)
		}
		return nil, "", fmt.Errorf("host service is shut down")
	}
	if _, exists := s.sessions[sessionID]; exists {
		s.mu.Unlock()
		if stopErr := b.Stop(); stopErr != nil {
			s.log.Printf("warning: stop backend for duplicate session %s: %v", sessionID, stopErr)
		}
		return nil, "", fmt.Errorf("session %s already registered", sessionID)
	}
	s.sessions[sessionID] = b
	s.mu.Unlock()

	// Persist initial metadata. Errors are logged, not surfaced —
	// rolling back a running backend is worse UX than an unpersisted
	// row.
	if s.sessionsStore != nil {
		now := time.Now()
		info := agent.SessionInfo{
			ID:         sessionID,
			ExternalID: req.SessionID, // empty for fresh; populated for resume
			Backend:    req.Backend,
			Status:     agent.StatusStarting,
			GitRef:     req.GitRef,
			Prompt:     req.Prompt,
			TicketID:   req.TicketID,
			Agent:      req.Agent,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
			s.log.Printf("warning: persist session %s metadata: %v", sessionID, err)
		}
	}

	// Sole drain on b.Events(); subscribers fan out to SSE handlers.
	s.wg.Add(1)
	go s.relayBackendEvents(sessionID, b)

	// Per-session serverURL is OpenCode-only; CreateBackend already
	// ensured a server exists for workDir.
	serverURL := ""
	if oc, ok := mgr.(*OpenCodeBackendManager); ok {
		for _, srv := range oc.ListServers() {
			if srv.ProjectDir == workDir {
				serverURL = srv.URL
				break
			}
		}
	}

	return b, serverURL, nil
}

// relayBackendEvents drains backend.Events() into the subscriber
// registry and applies metadata side-effects. Exits when Events()
// closes; tracked by s.wg so Shutdown can wait it out.
func (s *Service) relayBackendEvents(sessionID string, b agent.SessionBackend) {
	defer s.wg.Done()
	for evt := range b.Events() {
		evt.SessionID = sessionID
		s.subscribers.Broadcast(evt)
		s.applyEventToMetadata(sessionID, evt)
	}
}

// applyEventToMetadata persists status/title changes and the first-
// time ExternalID stamp (Claude only learns its remote session ID
// mid-stream during Open; if the daemon dies before Open returns the
// binding would be lost).
//
// UpdatedAt only bumps on a user-visible change so MarkRead stays
// sticky against the steady stream of backend events.
func (s *Service) applyEventToMetadata(sessionID string, evt agent.Event) {
	if s.sessionsStore == nil {
		return
	}
	hasExternalID := evt.ExternalID != ""
	hasStatus := false
	var statusValue agent.SessionStatus
	if evt.Type == agent.EventStatusChange {
		if d, ok := evt.Data.(agent.StatusChangeData); ok {
			hasStatus = true
			statusValue = d.NewStatus
		}
	}
	hasTitle := false
	var titleValue string
	if evt.Type == agent.EventTitleChange {
		if d, ok := evt.Data.(agent.TitleChangeData); ok {
			hasTitle = true
			titleValue = d.Title
		}
	}
	if !hasExternalID && !hasStatus && !hasTitle {
		return
	}

	ctx := context.Background()
	info, err := s.sessionsStore.GetSession(ctx, sessionID)
	if errors.Is(err, store.ErrSessionNotFound) {
		// Out-of-band session (e.g. tests that didn't pre-persist).
		return
	}
	if err != nil {
		// A real DB error here would silently lose the first-time
		// ExternalID stamp; log so a daemon-side outage is visible in
		// the host's stderr instead of disappearing into the relay.
		s.log.Printf("warning: load session %s metadata for %s event: %v", sessionID, evt.Type, err)
		return
	}

	dirty := false
	if hasExternalID && info.ExternalID == "" {
		info.ExternalID = evt.ExternalID
		dirty = true
	}
	if hasStatus && info.Status != statusValue {
		info.Status = statusValue
		dirty = true
	}
	if hasTitle && info.Title != titleValue {
		info.Title = titleValue
		dirty = true
	}
	if !dirty {
		return
	}
	info.UpdatedAt = time.Now()
	if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
		s.log.Printf("warning: update session %s metadata for %s event: %v", sessionID, evt.Type, err)
	}
}

// workDirFor resolves a GitRef to an absolute working directory.
// Precedence: usable LocalPath → clone of RemoteURL into
// <clonesDir>/<CloneDirName>/ → error. WorktreeBranch is resolved
// under the base when set.
//
// LocalPath failing soft (missing / not a repo) falls through to a
// remote clone; it's a hard error only when no RemoteURL is set.
func (s *Service) workDirFor(ctx context.Context, ref agent.GitRef) (string, error) {
	var base string

	if ref.LocalPath != "" {
		res := s.tryLocalPath(ref.LocalPath)
		if res.HardErr != nil {
			return "", res.HardErr
		}
		if res.Usable {
			base = ref.LocalPath
		} else if ref.RemoteURL == "" {
			return "", fmt.Errorf("local_path %q not usable: %w", ref.LocalPath, res.SoftFail)
		}
	}

	if base == "" {
		if ref.RemoteURL == "" {
			return "", fmt.Errorf("git ref must set at least one of local_path or remote_url")
		}
		if s.clonesDir == "" {
			return "", fmt.Errorf("cannot resolve remote ref: host has no clones_dir configured")
		}
		name, err := agent.CloneDirName(ref.RemoteURL)
		if err != nil {
			return "", fmt.Errorf("clone dir name for %q: %w", ref.RemoteURL, err)
		}
		base = filepath.Join(s.clonesDir, name)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			// Singleflight so concurrent first-uses don't race on
			// `git clone` into the same dir.
			_, cloneErr, _ := s.cloneSF.Do(base, func() (any, error) {
				// Re-check inside the singleflight in case a peer
				// finished while we were queued.
				if _, statErr := os.Stat(base); statErr == nil {
					return nil, nil
				}
				if mkErr := os.MkdirAll(s.clonesDir, 0o755); mkErr != nil {
					return nil, fmt.Errorf("create clones dir %q: %w", s.clonesDir, mkErr)
				}
				s.log.Printf("cloning %s into %s", ref.RemoteURL, base)
				if cloneErr := git.CloneWithConfig(ref.RemoteURL, base, nil); cloneErr != nil {
					return nil, fmt.Errorf("clone %q: %w", ref.RemoteURL, cloneErr)
				}
				return nil, nil
			})
			if cloneErr != nil {
				return "", cloneErr
			}
		} else if err != nil {
			return "", fmt.Errorf("stat clone dir %q: %w", base, err)
		}
	}

	if ref.WorktreeBranch != "" {
		wt, err := s.resolveWorktree(ctx, base, ref.WorktreeBranch)
		if err != nil {
			return "", fmt.Errorf("resolve worktree for branch %q: %w", ref.WorktreeBranch, err)
		}
		return wt.WorktreeDir, nil
	}
	return base, nil
}

// localPathResult is the outcome of tryLocalPath. Exactly one field
// is meaningful:
//   - Usable: path is the repo root, use it directly.
//   - SoftFail: path missing or not a git repo — caller may fall back
//     to a remote clone.
//   - HardErr: caller bug (relative path, not the repo root, etc.) —
//     never fall back.
type localPathResult struct {
	Usable   bool
	SoftFail error
	HardErr  error
}

// tryLocalPath checks whether path is usable as a session work
// directory; see localPathResult for the field semantics.
func (s *Service) tryLocalPath(path string) localPathResult {
	if !filepath.IsAbs(path) {
		return localPathResult{HardErr: fmt.Errorf("local_path must be absolute, got %q", path)}
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return localPathResult{SoftFail: fmt.Errorf("path does not exist on this host")}
		}
		return localPathResult{HardErr: fmt.Errorf("stat local_path %q: %w", path, err)}
	}
	root, err := git.RepoRoot(path)
	if err != nil {
		return localPathResult{SoftFail: fmt.Errorf("not a git repo")}
	}
	// EvalSymlinks both sides — macOS reports /var/folders as
	// /private/var/folders for the root.
	givenAbs, err := filepath.EvalSymlinks(path)
	if err != nil {
		return localPathResult{HardErr: fmt.Errorf("resolve symlinks for %q: %w", path, err)}
	}
	rootAbs, err := filepath.EvalSymlinks(root)
	if err != nil {
		return localPathResult{HardErr: fmt.Errorf("resolve symlinks for repo root %q: %w", root, err)}
	}
	if filepath.Clean(rootAbs) != filepath.Clean(givenAbs) {
		return localPathResult{HardErr: fmt.Errorf("local_path %q is not the repo root (root is %q)", path, root)}
	}
	return localPathResult{Usable: true}
}

// Session returns the live SessionBackend for id, or (nil, false).
// Does NOT rehydrate — callers that need cross-restart resume use
// ensureBackend (via the typed live-session ops below).
func (s *Service) Session(id string) (agent.SessionBackend, bool) {
	s.mu.RLock()
	b, ok := s.sessions[id]
	s.mu.RUnlock()
	return b, ok
}

// ensureBackend returns the live backend for id, lazily rebuilding
// the wrapper from the persisted store row if the registry missed.
// Without this lazy rebuild every session-op would 404 after a daemon
// restart until the user manually recreated the session.
//
// Rebuild only recreates the Go-side wrapper (SDK client + event
// channel); the agent subprocess and its session DB are untouched —
// the wrapper is pointed at the persisted ExternalID via
// BackendInvocation.ResumeExternalID.
//
// Returns ErrNotFound when id is in neither the registry nor the
// store. Real store errors (DB lock, disk full) are surfaced wrapped,
// not coerced into ErrNotFound — same pattern as GetSessionMetadata.
func (s *Service) ensureBackend(ctx context.Context, id string) (agent.SessionBackend, error) {
	if b, ok := s.Session(id); ok {
		return b, nil
	}
	if s.sessionsStore == nil {
		return nil, ErrNotFound
	}
	info, err := s.sessionsStore.GetSession(ctx, id)
	if errors.Is(err, store.ErrSessionNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ensure backend %s: load session: %w", id, err)
	}

	mgr, ok := s.backendManagers[info.Backend]
	if !ok {
		return nil, fmt.Errorf("ensure backend %s: no backend manager for %s", id, info.Backend)
	}
	s.mu.RLock()
	closedBeforeWork := s.closed
	s.mu.RUnlock()
	if closedBeforeWork {
		return nil, fmt.Errorf("ensure backend %s: host is shut down", id)
	}

	workDir, err := s.workDirFor(ctx, info.GitRef)
	if err != nil {
		return nil, fmt.Errorf("ensure backend %s: %w", id, err)
	}
	b, err := mgr.CreateBackend(ctx, agent.BackendInvocation{
		WorkDir:          workDir,
		ResumeExternalID: info.ExternalID,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure backend %s: %w", id, err)
	}

	s.mu.Lock()
	if existing, ok := s.sessions[id]; ok {
		s.mu.Unlock()
		// Lost the race; tear down our spare backend.
		if stopErr := b.Stop(); stopErr != nil {
			s.log.Printf("warning: stop backend lost-race for %s: %v", id, stopErr)
		}
		return existing, nil
	}
	if s.closed {
		s.mu.Unlock()
		if stopErr := b.Stop(); stopErr != nil {
			s.log.Printf("warning: stop backend after shutdown for %s: %v", id, stopErr)
		}
		return nil, fmt.Errorf("ensure backend %s: host is shut down", id)
	}
	s.sessions[id] = b
	s.mu.Unlock()

	s.wg.Add(1)
	go s.relayBackendEvents(id, b)

	// Open is required by the SessionBackend contract — Send/Messages
	// fast-fail on an unopened backend. On Open failure tear down the
	// registration so the next call re-runs ensureBackend instead of
	// finding a broken wrapper in s.sessions.
	if err := b.Open(ctx); err != nil {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		if stopErr := b.Stop(); stopErr != nil {
			s.log.Printf("warning: stop backend after open failure for %s: %v", id, stopErr)
		}
		return nil, fmt.Errorf("ensure backend %s: open: %w", id, err)
	}

	return b, nil
}

// StopSession stops the SessionBackend registered under id and removes
// it from the registry. Returns ErrNotFound if there is no such session.
// Safe to call concurrently with reads.
func (s *Service) StopSession(id string) error {
	s.mu.Lock()
	b, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.sessions, id)
	s.mu.Unlock()
	return b.Stop()
}

// --- Live session ops ---------------------------------------------------
//
// Every action needing the in-memory backend wrapper goes through one
// of these. The mux/HTTP layer never touches s.sessions directly;
// ensureBackend handles lazy rehydration on first use after a restart.

// SendMessage dispatches opts to the session's live backend.
func (s *Service) SendMessage(ctx context.Context, id string, opts agent.SendMessageOpts) error {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return err
	}
	return b.Send(ctx, opts)
}

// AbortSession asks the agent to stop streaming.
func (s *Service) AbortSession(ctx context.Context, id string) error {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return err
	}
	return b.Abort(ctx)
}

// RevertSession truncates the conversation at messageID.
func (s *Service) RevertSession(ctx context.Context, id, messageID string) error {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return err
	}
	return b.Revert(ctx, messageID)
}

// ForkSession creates a sibling session forked off messageID.
func (s *Service) ForkSession(ctx context.Context, id, messageID string) (agent.ForkResult, error) {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return agent.ForkResult{}, err
	}
	return b.Fork(ctx, messageID)
}

// SessionMessages returns the conversation history.
func (s *Service) SessionMessages(ctx context.Context, id string) ([]agent.MessageData, error) {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return nil, err
	}
	return b.Messages(ctx)
}

// OpenSession ensures the backend is live and its SSE listener is
// attached. Returns the post-Open snapshot (status, external session
// id) — async-init backends like Claude only learn their session id
// inside Open. Idempotent.
func (s *Service) OpenSession(ctx context.Context, id string) (agent.SessionStatus, string, error) {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return "", "", err
	}
	if err := b.Open(ctx); err != nil {
		return "", "", err
	}
	return b.Status(), b.SessionID(), nil
}

// OpenAndSend opens the backend and dispatches opts as the initial
// turn (or a follow-up after resume).
func (s *Service) OpenAndSend(ctx context.Context, id string, opts agent.SendMessageOpts) (agent.SessionStatus, string, error) {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return "", "", err
	}
	if err := b.OpenAndSend(ctx, opts); err != nil {
		return "", "", err
	}
	return b.Status(), b.SessionID(), nil
}

// RespondPermission replies to a pending tool-use permission prompt.
func (s *Service) RespondPermission(ctx context.Context, id, permissionID string, allow bool) error {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return err
	}
	return b.RespondPermission(ctx, permissionID, allow)
}

// --- Worktree / branch ops ----------------------------------------------

// ListBranches returns the branches (and their checked-out worktrees)
// for the repository identified by ref. Skips bare and detached entries.
// ref.WorktreeBranch is ignored — listing operates on the repo root.
func (s *Service) ListBranches(ctx context.Context, ref agent.GitRef) ([]BranchInfo, error) {
	repoRef := ref
	repoRef.WorktreeBranch = ""
	root, err := s.workDirFor(ctx, repoRef)
	if err != nil {
		return nil, err
	}
	return s.listBranches(ctx, root)
}

// ResolveWorktree ensures a worktree exists for (ref's repo, branch) and
// returns its info. ref.WorktreeBranch is ignored — pass branch as a
// distinct argument so the caller's intent ("resolve THIS branch") is
// explicit at the call site.
func (s *Service) ResolveWorktree(ctx context.Context, ref agent.GitRef, branch string) (WorktreeInfo, error) {
	repoRef := ref
	repoRef.WorktreeBranch = ""
	root, err := s.workDirFor(ctx, repoRef)
	if err != nil {
		return WorktreeInfo{}, err
	}
	wt, err := s.resolveWorktree(ctx, root, branch)
	if err == nil {
		s.branches.invalidate(root)
	}
	return wt, err
}

// RemoveWorktree removes the worktree for (ref's repo, branch).
func (s *Service) RemoveWorktree(ctx context.Context, ref agent.GitRef, branch string, force bool) error {
	repoRef := ref
	repoRef.WorktreeBranch = ""
	root, err := s.workDirFor(ctx, repoRef)
	if err != nil {
		return err
	}
	if err := s.removeWorktree(ctx, root, branch, force); err != nil {
		return err
	}
	s.branches.invalidate(root)
	return nil
}

// MergeBranch merges branch into ref's repo's default branch.
func (s *Service) MergeBranch(ctx context.Context, ref agent.GitRef, branch, commitMessage string) (MergeResult, error) {
	repoRef := ref
	repoRef.WorktreeBranch = ""
	root, err := s.workDirFor(ctx, repoRef)
	if err != nil {
		return MergeResult{}, err
	}
	res, err := s.mergeBranch(ctx, root, branch, commitMessage)
	if err == nil {
		s.branches.invalidate(root)
	}
	return res, err
}

// listBranches lists branches (and their worktrees) at projectDir,
// skipping bare/detached. Cached per projectDir; mutating ops call
// branches.invalidate.
func (s *Service) listBranches(_ context.Context, projectDir string) ([]BranchInfo, error) {
	if cached, ok := s.branches.get(projectDir); ok {
		return cached, nil
	}

	worktrees, err := git.ListWorktrees(projectDir)
	if err != nil {
		return nil, err
	}

	defaultBranch, _ := git.DefaultBranch(projectDir)
	currentBranch, _ := git.CurrentBranch(projectDir)

	// Derive the repo label: use the remote name when available so that
	// forks of the same directory name are distinguishable; fall back to
	// the basename of projectDir for local-only repos.
	repoLabel := filepath.Base(projectDir)
	if remoteURL, err := git.RemoteURL(projectDir, "origin"); err == nil && remoteURL != "" {
		// Strip common git URL noise to produce a short, readable label.
		// e.g. "https://github.com/acme/api.git" → "api"
		//      "git@github.com:acme/api.git"     → "api"
		repoLabel = repoLabelFromURL(remoteURL, repoLabel)
	}

	result := make([]BranchInfo, 0, len(worktrees))
	for _, wt := range worktrees {
		if wt.Bare || wt.Branch == "" {
			continue
		}
		info := BranchInfo{
			Name:        wt.Branch,
			WorktreeDir: wt.Path,
			IsDefault:   wt.Branch == defaultBranch,
			IsCurrent:   wt.Branch == currentBranch,
			RepoLabel:   repoLabel,
		}
		// Diff stats + ahead count are only meaningful off-default.
		if wt.Branch != defaultBranch {
			if added, removed, err := git.DiffStat(wt.Path, defaultBranch); err == nil {
				info.LinesAdded = added
				info.LinesRemoved = removed
			}
			if ahead, err := git.CommitsAhead(projectDir, defaultBranch, wt.Branch); err == nil {
				info.CommitsAhead = ahead
			}
		}
		result = append(result, info)
	}
	s.branches.put(projectDir, result)
	return result, nil
}

// resolveWorktree ensures a worktree exists for (projectDir, branch),
// creating the branch off the default if missing. Refuses to *create*
// a worktree for the default branch (ErrReservedBranch) so the
// original checkout retains it; lookups of an existing default-branch
// worktree still succeed.
func (s *Service) resolveWorktree(_ context.Context, projectDir, branch string) (WorktreeInfo, error) {
	if strings.TrimSpace(branch) == "" {
		return WorktreeInfo{}, ErrInvalidBranchName
	}
	wt, err := git.FindWorktreeForBranch(projectDir, branch)
	if err != nil {
		return WorktreeInfo{}, err
	}
	if wt != nil {
		return WorktreeInfo{Branch: branch, WorktreeDir: wt.Path}, nil
	}

	// Reject the default branch here (post-lookup) so the lookup path
	// above keeps working for an existing default worktree.
	defaultBranch, err := git.DefaultBranch(projectDir)
	if err != nil {
		return WorktreeInfo{}, fmt.Errorf("determine default branch: %w", err)
	}
	if branch == defaultBranch {
		return WorktreeInfo{}, ErrReservedBranch
	}

	projectName := filepath.Base(projectDir)
	wtDir, err := git.WorktreeDir(projectName, branch)
	if err != nil {
		return WorktreeInfo{}, err
	}

	exists, err := git.BranchExists(projectDir, branch)
	if err != nil {
		return WorktreeInfo{}, err
	}

	if exists {
		if err := git.AddWorktree(projectDir, wtDir, branch); err != nil {
			return WorktreeInfo{}, err
		}
	} else {
		if err := git.AddWorktreeNewBranch(projectDir, wtDir, branch, defaultBranch); err != nil {
			return WorktreeInfo{}, err
		}
	}
	s.log.Printf("created worktree for branch %q at %s", branch, wtDir)
	return WorktreeInfo{Branch: branch, WorktreeDir: wtDir}, nil
}

// removeWorktree removes the worktree for (projectDir, branch). Returns
// an error if there is no such worktree.
func (s *Service) removeWorktree(_ context.Context, projectDir, branch string, force bool) error {
	wt, err := git.FindWorktreeForBranch(projectDir, branch)
	if err != nil {
		return err
	}
	if wt == nil {
		return fmt.Errorf("%w: no worktree found for branch %q", ErrNotFound, branch)
	}
	if err := git.RemoveWorktree(projectDir, wt.Path, force); err != nil {
		return err
	}
	s.log.Printf("removed worktree for branch %q at %s", branch, wt.Path)
	return nil
}

// MergeResult describes the outcome of MergeBranch.
type MergeResult struct {
	MergedBranch    string
	BranchWorktree  string // Path of the feature-branch worktree (empty if it was cleaned up)
	WorktreeRemoved bool
	BranchDeleted   bool
}

// mergeBranch merges `branch` into the repo's default branch. Before
// merging, it `git add -A`s the feature worktree; if there are staged
// changes, commitMessage is used to commit them first (required in that
// case).
func (s *Service) mergeBranch(_ context.Context, projectDir, branch, commitMessage string) (MergeResult, error) {
	defaultBranch, err := git.DefaultBranch(projectDir)
	if err != nil {
		return MergeResult{}, fmt.Errorf("determine default branch: %w", err)
	}
	if branch == defaultBranch {
		return MergeResult{}, ErrCannotMergeDefault
	}

	branchWt, err := git.FindWorktreeForBranch(projectDir, branch)
	if err != nil {
		return MergeResult{}, fmt.Errorf("find branch worktree: %w", err)
	}
	if branchWt == nil {
		return MergeResult{}, fmt.Errorf("%w: no worktree found for branch %q", ErrNotFound, branch)
	}

	if err := git.AddAll(branchWt.Path); err != nil {
		return MergeResult{}, fmt.Errorf("git add -A in worktree: %w", err)
	}
	hasStagedWork, err := git.HasStagedChanges(branchWt.Path)
	if err != nil {
		return MergeResult{}, fmt.Errorf("check staged changes: %w", err)
	}
	commitsAhead, err := git.CommitsAhead(projectDir, defaultBranch, branch)
	if err != nil {
		return MergeResult{}, fmt.Errorf("count commits ahead: %w", err)
	}
	if !hasStagedWork && commitsAhead == 0 {
		return MergeResult{}, ErrNothingToMerge
	}
	if hasStagedWork {
		if commitMessage == "" {
			return MergeResult{}, ErrCommitMessageRequired
		}
		if err := git.Commit(branchWt.Path, commitMessage); err != nil {
			return MergeResult{}, fmt.Errorf("commit worktree changes: %w", err)
		}
		s.log.Printf("committed worktree changes in %s on branch %q", branchWt.Path, branch)
	}

	mainWt, err := git.FindWorktreeForBranch(projectDir, defaultBranch)
	if err != nil {
		return MergeResult{}, fmt.Errorf("find main worktree: %w", err)
	}
	if mainWt == nil {
		return MergeResult{}, fmt.Errorf("%w: no worktree for default branch %q", ErrNotFound, defaultBranch)
	}
	clean, err := git.IsClean(mainWt.Path)
	if err != nil {
		return MergeResult{}, fmt.Errorf("check worktree clean: %w", err)
	}
	if !clean {
		return MergeResult{}, ErrTargetDirty
	}

	mergeMsg := fmt.Sprintf("Merge branch '%s'", branch)
	if err := git.MergeNoFF(mainWt.Path, branch, mergeMsg); err != nil {
		if git.IsMerging(mainWt.Path) {
			_ = git.AbortMerge(mainWt.Path)
			return MergeResult{}, ErrMergeConflict
		}
		return MergeResult{}, fmt.Errorf("merge failed: %w", err)
	}
	s.log.Printf("merged branch %q into %q in %s", branch, defaultBranch, mainWt.Path)

	res := MergeResult{
		MergedBranch:   branch,
		BranchWorktree: branchWt.Path,
	}
	if err := git.RemoveWorktree(projectDir, branchWt.Path, true); err != nil {
		s.log.Printf("warning: could not remove worktree after merge: %v", err)
	} else {
		res.WorktreeRemoved = true
	}
	if err := git.DeleteBranch(projectDir, branch, false); err != nil {
		s.log.Printf("warning: could not delete branch after merge: %v", err)
	} else {
		res.BranchDeleted = true
	}
	return res, nil
}
