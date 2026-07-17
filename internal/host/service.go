package host

// Service is the Host plane's domain object: it owns BackendManagers
// for agent sessions and resolves GitRefs to working directories.
// LocalPath refs use the path on this host directly; WorktreeID refs
// resolve to ~/work/<WorktreeID>/ — a linked `git worktree` of the
// repo's bare canonical clone at ~/work/repos/<slug>/repo.git (see
// repos.go). Worktrees are created by import (clone), scaffold
// (template), or fork/load (CreateRepoWorktree,
// repos_worktree_create.go).

import (
	"context"
	cryptoRandImpl "crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	githubpkg "github.com/acksell/clank/internal/host/github"
	"github.com/acksell/clank/internal/host/preview"
	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/internal/keepalive"
	"github.com/acksell/clank/internal/notifier"
	"github.com/acksell/clank/internal/repolabel"
)

// cryptoRand is the entropy source for worktree-ULID generation
// (CreateRepoWorktree, ImportProject). Aliased so callers don't need
// to remember the rename-on-import.
var cryptoRand = cryptoRandImpl.Reader

// Service is the Host plane's domain object. Construct with New; call
// Init to start background goroutines and Shutdown to release them.
// Owns a registry of live SessionBackends keyed by session ULID.
type Service struct {
	id              string
	startedAt       time.Time
	backendManagers map[agent.BackendType]agent.BackendManager
	auth            *AuthManager
	github          *githubpkg.Manager
	log             *log.Logger

	// Git committer identity stamped on a scaffolded project's seed
	// commit (CreateProjectFromTemplate). Defaulted in New.
	projectCommitterName  string
	projectCommitterEmail string
	templates             []Template

	mu       sync.RWMutex
	sessions map[string]agent.SessionBackend
	// closed gates new registrations. CreateSession re-checks after
	// the slow CreateBackend call to avoid leaking into a torn-down
	// registry.
	closed bool

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

	// keepaliveLoop signals "agent is active" to a provider-specific
	// Listener while events are flowing. Nil when Options.KeepaliveListener
	// is nil — laptop mode skips the goroutine and ticker entirely.
	keepaliveLoop *keepalive.Loop
	keepaliveStop context.CancelFunc

	// notifierLoop delivers push Notifications to a provider-specific
	// Provider (webhook, expo, …). The host decides what's push-worthy
	// and composes the copy (see startNotifier / classifyEvent); the
	// Loop only queues and sends. Nil when Options.NotifierLoop is nil
	// — no goroutine, no subscriber slot. Sibling of keepaliveLoop, NOT
	// a replacement — the two consume the same fan-out at different
	// granularity.
	notifierLoop *notifier.Loop
	notifierStop context.CancelFunc

	// preview owns per-worktree dev-server lifecycle (Metro for Expo
	// in v1). Constructed unconditionally in New — preview is a pure
	// in-memory feature with no external dependencies, so there's
	// nothing to gate on.
	preview *preview.Manager

	// worktreeLocks serialize destructive per-worktree operations
	// (DeleteWorktree's unlink) against session creation: removing
	// ~/work/<id> while a session is resolving its workdir or starting
	// its backend would corrupt it. Keyed by worktree ID. See
	// lockWorktree.
	worktreeLocksMu sync.Mutex
	worktreeLocks   map[string]*sync.Mutex

	// repoLocks serialize canonical-repo mutations (clone, fetch,
	// worktree add/remove, branch create, publish's remote-add) per
	// repo slug — every ~/work/<id> worktree of a repo shares one bare
	// canonical, so concurrent ref/config writes must not interleave.
	// Same lazily-allocated shape as worktreeLocks. See lockRepo (repos.go).
	repoLocksMu sync.Mutex
	repoLocks   map[string]*sync.Mutex
}

// Options configures a Service at construction time.
type Options struct {
	// ID is the host identifier. Defaults to HostLocal when empty.
	ID string
	// BackendManagers maps each backend type to its manager. Required.
	BackendManagers map[agent.BackendType]agent.BackendManager
	// Log is the logger. Defaults to a logger writing to stderr with the
	// "[clank-host]" prefix.
	Log *log.Logger
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

	// KeepaliveListener forwards backend-event activity to a provider-
	// specific keep-alive mechanism (e.g. the Sprites Tasks API). Nil
	// disables the subsystem entirely — laptop mode default. Set by
	// cmd/clank-host/main.go from the --keepalive-provider flag.
	KeepaliveListener keepalive.Listener

	// NotifierLoop, when set, delivers the host's push Notifications to
	// an outbound Provider (webhook/expo/noop). Nil disables the
	// subsystem — laptop mode default. Construct via notifier.New in
	// cmd/clank-host/main.go.
	NotifierLoop *notifier.Loop

	// PreviewGWClient calls the gateway's preview register/revoke
	// webhooks to mint and tear down public tokens. Nil (or a client
	// constructed with empty URL) keeps preview spawns local-only —
	// the dev server runs but Status.Token/URL stay empty.
	PreviewGWClient *preview.GWClient

	// GitHubOAuthClientID is the Clank GitHub OAuth App's client_id,
	// used by the host's GitHub Connect device flow. Empty disables
	// the connect surface (status reports available:false). When
	// non-empty, takes precedence over the CLANK_GITHUB_OAUTH_CLIENT_ID
	// env var the laptop's clank-host inherits from clankd.
	GitHubOAuthClientID string

	// GitHubGhCLIAuth lets GitHub token resolution fall back to the
	// machine's own gh CLI login (`gh auth token`) when no clank
	// connection exists. Set by the local laptop provisioner — the
	// host IS the user's machine there; remote sandboxes keep token
	// access explicit.
	GitHubGhCLIAuth bool

	// ProjectCommitterName / ProjectCommitterEmail set the git committer
	// identity stamped on the seed commit of a project scaffolded via
	// CreateProjectFromTemplate (also persisted as the new repo's local
	// git identity). Attribution is operator branding, so the deploy-time
	// caller (e.g. clank-host's --project-committer-* flags) injects the
	// real values; empty falls back to a neutral default in New, keeping
	// any vendor identity out of this OSS package.
	ProjectCommitterName  string
	ProjectCommitterEmail string

	// Templates is the operator-configured builtin half of the
	// create-project catalog, served by GET /templates alongside the
	// user's own GitHub template repos. Empty means no builtin
	// templates (github-only or none).
	Templates []Template
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
	committerName := opts.ProjectCommitterName
	if committerName == "" {
		committerName = defaultProjectCommitterName
	}
	committerEmail := opts.ProjectCommitterEmail
	if committerEmail == "" {
		committerEmail = defaultProjectCommitterEmail
	}
	s := &Service{
		id:                    id,
		startedAt:             time.Now(),
		backendManagers:       opts.BackendManagers,
		log:                   lg,
		projectCommitterName:  committerName,
		projectCommitterEmail: committerEmail,
		templates:             opts.Templates,
		sessions:              make(map[string]agent.SessionBackend),
		branches:              newBranchCache(opts.BranchCacheTTL, opts.Now),
		sessionsStore:         opts.SessionsStore,
		subscribers:           newSubscriberRegistry(),
		worktreeLocks:         make(map[string]*sync.Mutex),
		repoLocks:             make(map[string]*sync.Mutex),
	}
	if opts.KeepaliveListener != nil {
		s.keepaliveLoop = keepalive.New(keepalive.Config{
			Listener: opts.KeepaliveListener,
			Log:      lg,
		})
	}
	if opts.NotifierLoop != nil {
		s.notifierLoop = opts.NotifierLoop
	}

	// Preview manager. No keepalive wiring — Fly's per-machine
	// hibernation tracks active connections (open HMR WebSocket counts),
	// and the prompt-box SSE through clank-host is the steady-state
	// activity signal anyway. If hibernation ever bites mid-HMR,
	// reintroduce a Bump callback on preview.Options.
	//
	// GWClient is what registers each spawned dev server with the
	// gateway so a public tokenized URL gets minted; nil / disabled
	// keeps spawning local-only.
	s.preview = preview.New(preview.Options{Log: lg, GWClient: opts.PreviewGWClient})

	// AuthManager handles credentials for every backend that has
	// connectable providers (OpenCode + Anthropic today). The restart
	// callback fires only on OpenCode credential writes — OpenCode
	// needs its servers re-cycled to pick up the new auth.json, but
	// Anthropic credentials just sit in env vars consumed by the next
	// claude spawn, no in-place reload needed.
	var restart func(ctx context.Context) error
	if oc, ok := s.backendManagers[agent.BackendOpenCode].(*OpenCodeBackendManager); ok {
		restart = func(ctx context.Context) error {
			return oc.ServerManager().RestartAllServers(ctx)
		}
	}
	am, err := NewAuthManager(restart)
	if err != nil {
		s.log.Printf("auth manager unavailable: %v", err)
	} else {
		s.auth = am
		// Wire claude-code to read its env vars from this AuthManager.
		// Resolved per-session so a mid-day credential change applies
		// to the next new session without a daemon restart.
		if cbm, ok := s.backendManagers[agent.BackendClaudeCode].(*ClaudeBackendManager); ok {
			cbm.SetEnvResolver(func() map[string]string {
				return am.AnthropicEnv()
			})
		}
	}

	// GitHub Connect: one manager per host. ClientID prefers the
	// Options field (set by clank-host's --github-oauth-client-id
	// flag); empty Options value falls back to the env var so laptop
	// dev runs that didn't pass the flag still work. Empty in both
	// places is a valid state — the manager reports available:false
	// and the UI hides the connect entry.
	clientID := opts.GitHubOAuthClientID
	if clientID == "" {
		clientID = os.Getenv(githubpkg.ClientIDEnv)
	}
	if home, herr := os.UserHomeDir(); herr != nil {
		s.log.Printf("github manager unavailable: %v", herr)
	} else {
		s.github = githubpkg.NewManager(home, clientID)
		if opts.GitHubGhCLIAuth {
			s.github.EnableGhCLIFallback()
		}
	}

	return s
}

// Auth returns the AuthManager, or nil when the OpenCode backend
// isn't registered. Callers must nil-check.
func (s *Service) Auth() *AuthManager { return s.auth }

// GitHub returns the GitHub Connect manager, or nil when home-dir
// resolution failed at construction. Callers must nil-check.
func (s *Service) GitHub() *githubpkg.Manager { return s.github }

// ID returns the host's ID.
func (s *Service) ID() string { return s.id }

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

	s.startKeepalive()
	s.startNotifier()

	// One-shot fast-forward of clean GitHub-backed worktrees — catches a
	// woken sprite up to whatever was pushed while it slept. See
	// remote_autopull.go.
	s.startColdStartAutoPull(ctx)

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

// normalizeStaleSessionStatus rewrites busy/starting/dead/error sessions
// back to idle on startup — none can advance without the live backend that
// set them, and that backend died with the previous process. error is reset
// too so a transient failure (e.g. a session opened before its worktree
// finished materializing) doesn't strand the session permanently red; the
// next open retries it. idle is already stable and left alone.
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
		case agent.StatusBusy, agent.StatusStarting, agent.StatusDead, agent.StatusError:
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
	if s.preview != nil {
		s.preview.Shutdown()
	}
	s.stopKeepalive()
	s.stopNotifier()
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
		ref := agent.GitRef{LocalPath: snap.Directory}
		// Best-effort: discovery records sessions wherever the backend
		// ran them; an odd path must not block registration.
		if norm, err := s.normalizeGitRef(ref); err == nil {
			ref = norm
		}
		info := agent.SessionInfo{
			ID:              ulid.Make().String(),
			ExternalID:      snap.ID,
			Backend:         snap.Backend,
			Status:          agent.StatusIdle,
			Title:           snap.Title,
			RevertMessageID: snap.RevertMessageID,
			GitRef:          ref,
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
// Returns a SessionInfo snapshot: req.GitRef is normalized (a
// LocalPath inside a repo becomes {root, Subdir}), and ServerURL is
// populated for backends with an HTTP server (OpenCode only) but never
// persisted — it's process-local. Persisting the rest is attempted
// when a store is configured and is best-effort: a write failure is
// logged, not surfaced, since rolling back a running backend is worse
// UX than an unpersisted row. The session's working directory is
// workDirFor of the normalized ref.
func (s *Service) CreateSession(ctx context.Context, sessionID string, req agent.StartRequest) (agent.SessionBackend, agent.SessionInfo, error) {
	if sessionID == "" {
		return nil, agent.SessionInfo{}, fmt.Errorf("session id is required")
	}
	if req.Backend == "" {
		return nil, agent.SessionInfo{}, fmt.Errorf("backend is required")
	}
	mgr, ok := s.backendManagers[req.Backend]
	if !ok {
		return nil, agent.SessionInfo{}, fmt.Errorf("no backend manager for %s", req.Backend)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, agent.SessionInfo{}, fmt.Errorf("host service is shut down")
	}
	if _, exists := s.sessions[sessionID]; exists {
		s.mu.Unlock()
		return nil, agent.SessionInfo{}, fmt.Errorf("session %s already registered", sessionID)
	}
	s.mu.Unlock()

	if err := req.GitRef.Validate(); err != nil {
		return nil, agent.SessionInfo{}, fmt.Errorf("git_ref: %w", err)
	}
	normRef, err := s.normalizeGitRef(req.GitRef)
	if err != nil {
		return nil, agent.SessionInfo{}, fmt.Errorf("git_ref: %w", err)
	}
	req.GitRef = normRef
	// Serialize against a concurrent DeleteWorktree of the same
	// worktree: removing ~/work/<id> while this session is resolving
	// its workdir / starting its backend would corrupt it. Held for
	// the rest of CreateSession so the delete waits until the session
	// is registered (then it sees it via WorktreeHasActiveSession).
	if wtID := req.GitRef.WorktreeID; wtID != "" {
		defer s.lockWorktree(wtID)()
	}
	workDir, err := s.workDirFor(ctx, req.GitRef)
	if err != nil {
		return nil, agent.SessionInfo{}, err
	}

	b, err := mgr.CreateBackend(ctx, agent.BackendInvocation{
		WorkDir:          workDir,
		ResumeExternalID: req.SessionID,
	})
	if err != nil {
		return nil, agent.SessionInfo{}, err
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
		return nil, agent.SessionInfo{}, fmt.Errorf("host service is shut down")
	}
	if _, exists := s.sessions[sessionID]; exists {
		s.mu.Unlock()
		if stopErr := b.Stop(); stopErr != nil {
			s.log.Printf("warning: stop backend for duplicate session %s: %v", sessionID, stopErr)
		}
		return nil, agent.SessionInfo{}, fmt.Errorf("session %s already registered", sessionID)
	}
	s.sessions[sessionID] = b
	s.mu.Unlock()

	now := time.Now()
	info := agent.SessionInfo{
		ID:         sessionID,
		ExternalID: req.SessionID, // empty for fresh; populated for resume
		Backend:    req.Backend,
		Status:     agent.StatusStarting,
		Hostname:   req.Hostname,
		GitRef:     req.GitRef,
		Prompt:     req.Prompt,
		TicketID:   req.TicketID,
		Agent:      req.Agent,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	// Persist initial metadata. Errors are logged, not surfaced —
	// rolling back a running backend is worse UX than an unpersisted
	// row.
	if s.sessionsStore != nil {
		if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
			s.log.Printf("warning: persist session %s metadata: %v", sessionID, err)
		}
		s.subscribers.Broadcast(agent.Event{
			Type:       agent.EventSessionCreate,
			SessionID:  sessionID,
			ExternalID: req.SessionID,
			Timestamp:  now,
			Data:       agent.MetaChangeData{Session: info},
		})
	}

	// Sole drain on b.Events(); subscribers fan out to SSE handlers.
	s.wg.Add(1)
	go s.relayBackendEvents(sessionID, b)

	// Per-session serverURL is OpenCode-only; CreateBackend already
	// ensured a server exists for workDir.
	if oc, ok := mgr.(*OpenCodeBackendManager); ok {
		for _, srv := range oc.ListServers() {
			if srv.ProjectDir == workDir {
				info.ServerURL = srv.URL
				break
			}
		}
	}

	return b, info, nil
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
// binding would be lost). On a successful write it also broadcasts
// EventMetaChange so subscribers (notably the TUI sidebar) receive
// the full post-mutation SessionInfo — including the bumped
// UpdatedAt that drives sidebar re-sort and unread state. This is
// the row-level counterpart to the field-level EventStatusChange /
// EventTitleChange that ran first in relayBackendEvents.
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
	// A new message is real activity even when the status doesn't move (e.g.
	// the backend appends a message without an idle/busy flip), so it bumps
	// UpdatedAt to keep recency sorting honest. Only the completed-message
	// event counts — EventPart (per-token deltas) would churn UpdatedAt on
	// every token, defeating the "user-visible change only" guarantee above.
	hasMessage := evt.Type == agent.EventMessage
	if !hasExternalID && !hasStatus && !hasTitle && !hasMessage {
		return
	}

	ctx := context.Background()
	// TODO(perf-debug): remove once daemon-latency investigation lands.
	// Each backend event triggers a GET + UPSERT against the single-
	// connection sqlite pool — these are top suspects for the 0.2-5s
	// stalls seen on /sessions. Logs each phase separately so we can
	// tell read vs write contention apart.
	getStart := time.Now()
	info, err := s.sessionsStore.GetSession(ctx, sessionID)
	if elapsed := time.Since(getStart); elapsed > sessionMetaSlowQueryThreshold {
		s.log.Printf("perf: GetSession(%s) for %s event took %s", sessionID, evt.Type, elapsed)
	}
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
	// StatusStarting is the transient state during backend Open() — it
	// flips back to Idle (or Error) within ~1-2s of the SDK connection
	// completing. Persisting it would bump UpdatedAt and broadcast
	// EventMetaChange, causing the sidebar to spuriously hoist + spinner
	// the session every time it lazy-resumes a backend (e.g. on
	// session-view open after a daemon restart). The session view still
	// receives the raw EventStatusChange via the SSE stream for its
	// "Connecting..." affordance; only the row's last-known stable
	// status is preserved here. normalizeStaleSessionStatus already
	// cleans up any Starting-at-rest that survives a daemon crash.
	if hasStatus && statusValue != agent.StatusStarting && info.Status != statusValue {
		info.Status = statusValue
		dirty = true
	}
	if hasTitle && info.Title != titleValue {
		info.Title = titleValue
		dirty = true
	}
	// A message carries no metadata field to diff against — its arrival is
	// itself the change, so it always marks the row dirty to bump UpdatedAt.
	if hasMessage {
		dirty = true
	}
	if !dirty {
		return
	}
	info.UpdatedAt = time.Now()
	upsertStart := time.Now()
	if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
		s.log.Printf("warning: update session %s metadata for %s event: %v", sessionID, evt.Type, err)
		// Skip the broadcast on a failed write — subscribers would
		// otherwise diverge from a subsequent Get() that returns the
		// pre-mutation row.
		return
	}
	if elapsed := time.Since(upsertStart); elapsed > sessionMetaSlowQueryThreshold {
		s.log.Printf("perf: UpsertSession(%s) for %s event took %s", sessionID, evt.Type, elapsed)
	}
	s.broadcastMetaChange(info)
}

// resolveRefDirs resolves ref's locator to the directory pair the two
// kinds of consumers need: base is the repo root (LocalPath refs) or
// the ~/work/<WorktreeID> worktree (worktree refs) — where git,
// branch, and worktree operations run; subdir is the working
// subdirectory relative to base ("" = base itself). WorktreeBranch is
// deliberately ignored here — branch worktrees are a workDirFor
// concern.
//
// Precedence (per the GitRef contract):
//  1. LocalPath set + inside a repo on this host → that repo's root.
//  2. WorktreeID set → ~/work/<WorktreeID>/. Errors with a clear
//     message if that directory is missing — the worktree must have
//     been created on this host (import/scaffold/CreateRepoWorktree)
//     first. We do NOT fall back to cloning.
//  3. Neither set / not usable → error.
func (s *Service) resolveRefDirs(ref agent.GitRef) (base, subdir string, err error) {
	if ref.LocalPath != "" {
		// Resolve the root from LocalPath alone — joining Subdir here
		// would make repo-root callers (repoRootFor) fail whenever the
		// subdir is missing on disk, even though they never use it.
		// workDirFor validates/stats the subdir itself once applied.
		res := s.tryLocalPath(ref.LocalPath)
		if res.HardErr != nil {
			return "", "", res.HardErr
		}
		if res.Usable {
			return res.Root, filepath.Join(res.Subdir, ref.Subdir), nil
		}
		if ref.WorktreeID == "" {
			return "", "", fmt.Errorf("local_path %q not usable on this host (%w) and no worktree_id was provided", ref.LocalPath, res.SoftFail)
		}
	}

	if ref.WorktreeID == "" {
		return "", "", fmt.Errorf("git ref must set at least one of local_path or worktree_id")
	}
	root, err := workRootDir()
	if err != nil {
		return "", "", err
	}
	base = filepath.Join(root, ref.WorktreeID)
	fi, err := os.Stat(base)
	switch {
	case os.IsNotExist(err):
		// Wrap ErrNotFound so writeError can map to 404. Other
		// resolveRefDirs returns are caller-bug (relative paths, etc.)
		// and stay as 500.
		return "", "", fmt.Errorf("%w: worktree %s not present at %s on this host", ErrNotFound, ref.WorktreeID, base)
	case err != nil:
		return "", "", fmt.Errorf("stat worktree dir %q: %w", base, err)
	case !fi.IsDir():
		return "", "", fmt.Errorf("worktree %s path %q is not a directory", ref.WorktreeID, base)
	}
	return base, ref.Subdir, nil
}

// workDirFor resolves a GitRef to the absolute working directory a
// session or preview dev server runs in: resolveRefDirs' base,
// narrowed to Subdir when set. WorktreeBranch (when set) first
// resolves to an additional git worktree of the base repo; Subdir then
// applies inside that worktree.
func (s *Service) workDirFor(ctx context.Context, ref agent.GitRef) (string, error) {
	base, subdir, err := s.resolveRefDirs(ref)
	if err != nil {
		return "", err
	}
	dir := base
	if ref.WorktreeBranch != "" {
		wt, err := s.resolveWorktree(ctx, base, ref.WorktreeBranch)
		if err != nil {
			return "", fmt.Errorf("resolve worktree for branch %q: %w", ref.WorktreeBranch, err)
		}
		dir = wt.WorktreeDir
	}
	if subdir == "" {
		return dir, nil
	}
	workDir := filepath.Join(dir, subdir)
	fi, err := os.Stat(workDir)
	switch {
	case os.IsNotExist(err):
		return "", fmt.Errorf("subdir %q does not exist under %s", subdir, dir)
	case err != nil:
		return "", fmt.Errorf("stat subdir %q: %w", workDir, err)
	case !fi.IsDir():
		return "", fmt.Errorf("subdir path %q is not a directory", workDir)
	}
	return workDir, nil
}

// repoRootFor resolves ref to the directory git, branch, and worktree
// operations run against: the repo root for LocalPath refs, the
// ~/work/<WorktreeID> worktree otherwise. Ignores WorktreeBranch and
// Subdir — those narrow the working directory (workDirFor), never the
// repo identity.
func (s *Service) repoRootFor(ref agent.GitRef) (string, error) {
	base, _, err := s.resolveRefDirs(ref)
	return base, err
}

// normalizeGitRef canonicalizes a usable LocalPath into the repo root
// plus a relative Subdir, so persisted identity (project_dir, RepoKey,
// sidebar grouping) always keys on the root while the session still
// runs at the requested folder. Unusable LocalPaths (SoftFail) pass
// through unchanged — the worktree_id fallback handles them. When the
// requested folder is a subdirectory and the client set no
// DisplayName, the folder's basename becomes the DisplayName so UIs
// keep showing the folder the user actually started in.
func (s *Service) normalizeGitRef(ref agent.GitRef) (agent.GitRef, error) {
	if ref.LocalPath == "" {
		return ref, nil
	}
	requested := filepath.Join(ref.LocalPath, ref.Subdir)
	res := s.tryLocalPath(requested)
	if res.HardErr != nil {
		return agent.GitRef{}, res.HardErr
	}
	if !res.Usable {
		return ref, nil
	}
	if res.Subdir != "" && ref.DisplayName == "" {
		ref.DisplayName = filepath.Base(requested)
	}
	ref.LocalPath = res.Root
	ref.Subdir = res.Subdir
	return ref, nil
}

// workRootForTest, when non-empty, overrides the $HOME/work parent
// for worktrees. Test-only hook — production callers leave this empty
// and rely on $HOME via os.UserHomeDir(). Avoids t.Setenv in
// parallel-heavy test packages.
//
// TODO(coderabbit): add sync.RWMutex if any test ever runs SetWorkRootForTest with t.Parallel()
// https://github.com/Acksell/clank/pull/16#discussion_r3213461979
var workRootForTest string

// workRootDir returns $HOME/work — the parent under which worktrees
// land at /<WorktreeID>/ (and repo canonicals under /repos/).
func workRootDir() (string, error) {
	if workRootForTest != "" {
		return workRootForTest, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, "work"), nil
}

// SetWorkRootForTest overrides workRootDir's lookup for the duration
// of a test. Returns the previous value so the caller can restore it
// in cleanup. Concurrent test access is unsafe — the override is a
// package-level singleton, so callers must serialize tests that use it.
func SetWorkRootForTest(path string) string {
	prev := workRootForTest
	workRootForTest = path
	return prev
}

// localPathResult is the outcome of tryLocalPath. Exactly one of the
// three states is meaningful:
//   - Usable: path is inside a git repo. Root is the repo root and
//     Subdir the path's location relative to it ("" when path IS the
//     root) — Root for git/identity, Join(Root, Subdir) as the cwd.
//   - SoftFail: path missing or not a git repo — caller may fall back
//     to a worktree_id.
//   - HardErr: caller bug (relative path, unresolvable symlinks) —
//     never fall back.
type localPathResult struct {
	Usable bool
	// Root is the repo root, in the caller's own path spelling where
	// verifiable (git's spelling otherwise — see rootInCallerSpelling).
	Root string
	// Subdir is path relative to Root; "" when path is the root.
	Subdir   string
	SoftFail error
	HardErr  error
}

// tryLocalPath checks whether path is usable as a session working
// directory; see localPathResult for the field semantics. Any
// directory inside a git repo is accepted — the repo root is derived
// from the path, not required of it.
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
	rel, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(givenAbs))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Impossible for a root derived from path itself; guard anyway.
		return localPathResult{HardErr: fmt.Errorf("local_path %q resolves outside its repo root %q", path, root)}
	}
	if rel == "." {
		return localPathResult{Usable: true, Root: path}
	}
	return localPathResult{Usable: true, Root: rootInCallerSpelling(path, root, rel, rootAbs), Subdir: rel}
}

// rootInCallerSpelling returns the repo root spelled the way the
// caller spelled path (e.g. /var/... instead of macOS's resolved
// /private/var/...), by trimming rel's components off path. Falls back
// to git's own spelling when the trimmed candidate doesn't resolve to
// the same directory — an intermediate component of path was a
// symlink, so pure trimming can't reconstruct the root.
func rootInCallerSpelling(path, gitRoot, rel, rootAbs string) string {
	candidate := filepath.Clean(path)
	for range strings.SplitSeq(rel, string(filepath.Separator)) {
		candidate = filepath.Dir(candidate)
	}
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil && filepath.Clean(resolved) == filepath.Clean(rootAbs) {
		return candidate
	}
	return gitRoot
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

// lockWorktree acquires the per-worktree lock and returns its release
// func. DeleteWorktree and CreateSession both take it so a destructive
// removal and a session start on the same worktree never interleave.
// Usage: defer lockWorktree(id)().
func (s *Service) lockWorktree(worktreeID string) func() {
	s.worktreeLocksMu.Lock()
	mu := s.worktreeLocks[worktreeID]
	if mu == nil {
		mu = &sync.Mutex{}
		s.worktreeLocks[worktreeID] = mu
	}
	s.worktreeLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// WorktreeHasActiveSession reports whether any session for worktreeID is
// currently running (busy or starting) on this host. DeleteWorktree and
// DeleteRepo use it to refuse removing a worktree with live work. A
// Service without a sessions store (test wiring) reports false.
func (s *Service) WorktreeHasActiveSession(ctx context.Context, worktreeID string) (bool, error) {
	if s.sessionsStore == nil {
		return false, nil
	}
	sessions, err := s.sessionsStore.ListSessionsByWorktree(ctx, worktreeID)
	if err != nil {
		return false, fmt.Errorf("list sessions for worktree %s: %w", worktreeID, err)
	}
	for _, info := range sessions {
		if isActiveSessionStatus(info.Status) {
			return true, nil
		}
		// Persisted status can lag a backend that just went busy; confirm
		// against the live registry as a backstop.
		if b, ok := s.Session(info.ID); ok && isActiveSessionStatus(b.Status()) {
			return true, nil
		}
	}
	return false, nil
}

func isActiveSessionStatus(st agent.SessionStatus) bool {
	return st == agent.StatusBusy || st == agent.StatusStarting
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
		// A cached backend whose connection has died (StatusDead) would make
		// every follow-up op fail forever — the "needs attention" wedge a user
		// hits after cancelling a turn, since the dead CLI transport lingers in
		// the registry. The chat-client spec relies on these paths to lazily
		// rehydrate (it omits /stop and /open by design), so drop the dead
		// backend and fall through to recreate it — mirroring the Open-failure
		// teardown below. Any other status is a live, reusable backend.
		if b.Status() != agent.StatusDead {
			// Open is idempotent and serialized per backend, so a caller that
			// lost the registration race to an in-flight Open blocks here
			// instead of getting back a backend whose client isn't set yet.
			if err := b.Open(ctx); err != nil {
				return nil, fmt.Errorf("ensure backend %s: open: %w", id, err)
			}
			return b, nil
		}
		if err := s.StopSession(id); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("ensure backend %s: drop dead backend: %w", id, err)
		}
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

	// Rehydrate = first touch after a daemon restart or backend drop.
	// Timed per phase: on a cold machine this path dominates session-
	// open latency (Open spawns the agent CLI), and the log line is how
	// we attribute it.
	rehydrateStart := time.Now()
	workDir, err := s.workDirFor(ctx, info.GitRef)
	if err != nil {
		return nil, fmt.Errorf("ensure backend %s: %w", id, err)
	}
	workDirDur := time.Since(rehydrateStart)
	createStart := time.Now()
	b, err := mgr.CreateBackend(ctx, agent.BackendInvocation{
		WorkDir:          workDir,
		ResumeExternalID: info.ExternalID,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure backend %s: %w", id, err)
	}
	createDur := time.Since(createStart)

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
	openStart := time.Now()
	if err := b.Open(ctx); err != nil {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		if stopErr := b.Stop(); stopErr != nil {
			s.log.Printf("warning: stop backend after open failure for %s: %v", id, stopErr)
		}
		s.log.Printf("rehydrate %s failed: backend=%s workdir=%s create=%s open=%s: %v",
			id, info.Backend, workDirDur.Round(time.Millisecond), createDur.Round(time.Millisecond),
			time.Since(openStart).Round(time.Millisecond), err)
		return nil, fmt.Errorf("ensure backend %s: open: %w", id, err)
	}
	s.log.Printf("rehydrate %s: backend=%s workdir=%s create=%s open=%s",
		id, info.Backend, workDirDur.Round(time.Millisecond), createDur.Round(time.Millisecond),
		time.Since(openStart).Round(time.Millisecond))

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

// ForkSession creates a sibling session forked off messageID and
// persists it as a new row so callers can navigate to it by the host's
// internal ID. Without this the wire response would only carry the
// backend's external session id (e.g. "ses_…"), which doesn't map to
// anything the host can look up.
func (s *Service) ForkSession(ctx context.Context, id, messageID string) (agent.SessionInfo, error) {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return agent.SessionInfo{}, err
	}
	fork, err := b.Fork(ctx, messageID)
	if err != nil {
		return agent.SessionInfo{}, err
	}

	src, err := s.sessionsStore.GetSession(ctx, id)
	if err != nil {
		return agent.SessionInfo{}, fmt.Errorf("fork: load source %s: %w", id, err)
	}
	now := time.Now()
	info := agent.SessionInfo{
		ID:         ulid.Make().String(),
		ExternalID: fork.ID,
		Backend:    src.Backend,
		Status:     agent.StatusIdle,
		Hostname:   src.Hostname,
		GitRef:     src.GitRef,
		Prompt:     src.Prompt,
		TicketID:   src.TicketID,
		Agent:      src.Agent,
		Title:      fork.Title,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
		return agent.SessionInfo{}, fmt.Errorf("fork: persist new session: %w", err)
	}
	return info, nil
}

// SessionMessages returns the conversation history. A live backend serves
// it directly. Without one, backends whose manager implements
// agent.TranscriptReader (Claude) are served straight from the on-disk
// transcript — no backend registration, no Open, no CLI spawn — so a pure
// history read never wakes the agent. Backends whose history API needs the
// live server (opencode) keep rehydrating via ensureBackend.
func (s *Service) SessionMessages(ctx context.Context, id string) ([]agent.MessageData, error) {
	// A dead backend is skipped, not used: reads don't repair the
	// registry — the next dispatching op (Send/Abort/…) rehydrates it.
	if b, ok := s.Session(id); ok && b.Status() != agent.StatusDead {
		return b.Messages(ctx)
	}
	if s.sessionsStore != nil {
		info, err := s.sessionsStore.GetSession(ctx, id)
		switch {
		case errors.Is(err, store.ErrSessionNotFound):
			// Fall through to ensureBackend for the ErrNotFound mapping.
		case err != nil:
			return nil, fmt.Errorf("session messages %s: load session: %w", id, err)
		default:
			if r, ok := s.backendManagers[info.Backend].(agent.TranscriptReader); ok {
				return s.readTranscript(ctx, r, info)
			}
		}
	}
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return nil, err
	}
	return b.Messages(ctx)
}

// readTranscript serves info's history from its backend's on-disk
// transcript via the manager's TranscriptReader capability.
func (s *Service) readTranscript(ctx context.Context, r agent.TranscriptReader, info agent.SessionInfo) ([]agent.MessageData, error) {
	// A fresh session that never opened has no transcript (and possibly
	// no external id ever) — empty history, not an error.
	if info.ExternalID == "" {
		return nil, nil
	}
	workDir, err := s.workDirFor(ctx, info.GitRef)
	if err != nil {
		return nil, fmt.Errorf("session messages %s: %w", info.ID, err)
	}
	return r.ReadTranscript(ctx, workDir, info.ExternalID)
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

// RespondPermission replies to a pending tool-use permission prompt. denyMessage
// is the reason forwarded to the model when allow is false (empty for a default).
func (s *Service) RespondPermission(ctx context.Context, id, permissionID string, allow bool, denyMessage string) error {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return err
	}
	return b.RespondPermission(ctx, permissionID, allow, denyMessage)
}

// RespondQuestion replies to a pending question prompt with structured
// answers (one per question, in order), or dismisses it when reject is true.
func (s *Service) RespondQuestion(ctx context.Context, id, requestID string, answers []agent.QuestionAnswer, reject bool) error {
	b, err := s.ensureBackend(ctx, id)
	if err != nil {
		return err
	}
	return b.RespondQuestion(ctx, requestID, answers, reject)
}

// --- Worktree / branch ops ----------------------------------------------

// ListBranches returns the branches (and their checked-out worktrees)
// for the repository identified by ref. Skips bare and detached entries.
// ref.WorktreeBranch is ignored — listing operates on the repo root.
func (s *Service) ListBranches(ctx context.Context, ref agent.GitRef) ([]BranchInfo, error) {
	root, err := s.repoRootFor(ref)
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
	root, err := s.repoRootFor(ref)
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
	root, err := s.repoRootFor(ref)
	if err != nil {
		return err
	}
	if err := s.removeWorktree(ctx, root, branch, force); err != nil {
		return err
	}
	s.branches.invalidate(root)
	return nil
}

// DeleteWorktree removes a worktree's persisted sessions and ~/work/<id>
// directory. Refuses with ErrWorktreeBusy when a session is active; idempotent otherwise.
//
// LOCK ORDER: repo lock (when the worktree is repo-first linked) BEFORE
// the per-worktree lock — the same order DeleteRepo uses, so the two
// can't ABBA-deadlock. The linked-ness probe runs before any lock:
// it's a read-only `git rev-parse` and the answer can't change under us
// (only this method and DeleteRepo unlink worktrees, both serialized by
// the repo lock).
func (s *Service) DeleteWorktree(ctx context.Context, worktreeID string) error {
	if _, err := ulid.ParseStrict(worktreeID); err != nil {
		return fmt.Errorf("delete worktree: invalid worktreeID %q", worktreeID)
	}
	root, err := workRootDir()
	if err != nil {
		return err
	}
	wtDir := filepath.Join(root, worktreeID)

	// Repo-first worktrees are linked `git worktree`s of a shared bare
	// canonical: remove them THROUGH git so the canonical's bookkeeping
	// (and the branch's checked-out lock) is released — a bare rm -rf
	// would strand a prunable stub and keep the branch unloadable. The
	// branch ref itself is kept on purpose: refs are cheap, the overview
	// keeps its history, and reloading the branch stays trivial.
	if gitDir, linked, cerr := worktreeCanonicalGitDir(wtDir); cerr == nil && linked {
		slug := filepath.Base(filepath.Dir(gitDir))
		defer s.lockRepo(slug)()
		return s.removeLinkedWorktree(ctx, worktreeID, wtDir, gitDir)
	}

	// Not a linked worktree: the dir is missing (already deleted —
	// idempotent no-op via RemoveAll) or corrupt (half-created, .git
	// unreadable) — plain removal. Serialize against session creation
	// and re-check for a live session under the worktree lock.
	defer s.lockWorktree(worktreeID)()
	active, err := s.WorktreeHasActiveSession(ctx, worktreeID)
	if err != nil {
		return err
	}
	if active {
		return ErrWorktreeBusy
	}
	if s.sessionsStore != nil {
		if err := s.sessionsStore.DeleteSessionsByWorktree(ctx, worktreeID); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(wtDir); err != nil {
		return fmt.Errorf("remove worktree %s: %w", worktreeID, err)
	}
	return nil
}

// removeLinkedWorktree is the shared deletion leg for a repo-first
// linked worktree: session purge + busy guard under the per-worktree
// lock, then git-aware removal. The CALLER holds the repo lock
// (lock order: repo → worktree).
func (s *Service) removeLinkedWorktree(ctx context.Context, worktreeID, wtDir, gitDir string) error {
	defer s.lockWorktree(worktreeID)()

	active, err := s.WorktreeHasActiveSession(ctx, worktreeID)
	if err != nil {
		return err
	}
	if active {
		return ErrWorktreeBusy
	}
	if s.sessionsStore != nil {
		if err := s.sessionsStore.DeleteSessionsByWorktree(ctx, worktreeID); err != nil {
			return err
		}
	}
	if err := git.RemoveWorktree(gitDir, wtDir, true); err != nil {
		return fmt.Errorf("remove worktree %s: %w", worktreeID, err)
	}
	if err := git.PruneWorktrees(gitDir); err != nil {
		s.log.Printf("warning: prune worktrees in %s: %v", gitDir, err)
	}
	return nil
}

// MergeBranch merges branch into ref's repo's default branch.
func (s *Service) MergeBranch(ctx context.Context, ref agent.GitRef, branch, commitMessage string) (MergeResult, error) {
	root, err := s.repoRootFor(ref)
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
		repoLabel = repolabel.RepoLabelFromURL(remoteURL, repoLabel)
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
