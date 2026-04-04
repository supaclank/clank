// Package daemon implements the Clank background daemon.
//
// The daemon manages coding agent sessions (OpenCode, Claude Code) as child
// processes, aggregates their events, and exposes an HTTP API over a Unix
// domain socket for the TUI and CLI to consume.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	"github.com/acksell/clank/internal/store"
	"github.com/acksell/clank/internal/voice"
	"github.com/acksell/mindmouth"
	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"
)

// OpenCodeBackendManager implements agent.BackendManager, agent.AgentLister,
// and agent.SessionDiscoverer for OpenCode sessions. It manages one OpenCode
// server per project directory via OpenCodeServerManager.
type OpenCodeBackendManager struct {
	serverMgr *agent.OpenCodeServerManager
}

// NewOpenCodeBackendManager creates a manager with a new server manager.
func NewOpenCodeBackendManager() *OpenCodeBackendManager {
	return &OpenCodeBackendManager{
		serverMgr: agent.NewOpenCodeServerManager(),
	}
}

// Init populates the desired server set from known project directories and
// starts the reconciler loop. The reconciler is the single owner of server
// lifecycle — it is the only code path that starts or stops servers.
// The first reconcile tick runs immediately, starting all known servers
// in parallel for fast startup.
func (m *OpenCodeBackendManager) Init(ctx context.Context, knownDirs func() ([]string, error)) error {
	dirs, err := knownDirs()
	if err != nil {
		return fmt.Errorf("load known dirs: %w", err)
	}
	var validDirs []string
	for _, dir := range dirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			log.Printf("[opencode] skipping desired dir %s: directory does not exist", dir)
			continue
		}
		validDirs = append(validDirs, dir)
	}
	if len(validDirs) > 0 {
		m.serverMgr.AddDesired(validDirs...)
		log.Printf("[opencode] added %d project dirs to desired set", len(validDirs))
	}

	go m.serverMgr.Run(ctx)
	return nil
}

// CreateBackend creates an OpenCode SessionBackend. It ensures an OpenCode
// server is running for the project directory before creating the backend.
// The backend receives a resolver closure that re-resolves the server URL
// on reconnect (handles server restarts on new ports).
func (m *OpenCodeBackendManager) CreateBackend(req agent.StartRequest) (agent.SessionBackend, error) {
	serverURL, err := m.serverMgr.GetOrStartServer(context.Background(), req.ProjectDir)
	if err != nil {
		return nil, fmt.Errorf("start opencode server for %s: %w", req.ProjectDir, err)
	}
	resolver := func(ctx context.Context) (string, error) {
		return m.serverMgr.GetOrStartServer(ctx, req.ProjectDir)
	}
	return agent.NewOpenCodeBackend(serverURL, req.SessionID, resolver), nil
}

// Shutdown stops all managed OpenCode servers.
func (m *OpenCodeBackendManager) Shutdown() {
	m.serverMgr.StopAll()
}

// ListAgents returns available agents for the given project directory.
func (m *OpenCodeBackendManager) ListAgents(ctx context.Context, projectDir string) ([]agent.AgentInfo, error) {
	return m.serverMgr.ListAgents(ctx, projectDir)
}

// ServerManager returns the underlying server manager. Exported for tests
// that need to configure the manager (e.g. injecting a fake startServerFn).
func (m *OpenCodeBackendManager) ServerManager() *agent.OpenCodeServerManager {
	return m.serverMgr
}

// ListServers returns running OpenCode server info from the server manager.
func (m *OpenCodeBackendManager) ListServers() []agent.ServerInfo {
	return m.serverMgr.ListServers()
}

// DiscoverSessions lists all projects from the OpenCode server, then lists
// all sessions using the same seed server. This avoids starting a new server
// per worktree (the old bug that caused triple-starts on startup).
//
// Worktrees that are clearly invalid (e.g. "/") are filtered out. Valid
// worktrees are added to the desired set so the reconciler starts servers
// for them (needed for future backend connections), but session listing
// uses the existing seed server.
func (m *OpenCodeBackendManager) DiscoverSessions(ctx context.Context, seedDir string) ([]agent.SessionSnapshot, error) {
	// Get the seed server URL — this will wait for the reconciler if needed.
	seedURL, err := m.serverMgr.GetOrStartServer(ctx, seedDir)
	if err != nil {
		return nil, fmt.Errorf("get seed server: %w", err)
	}

	projects, err := m.serverMgr.ListProjects(ctx, seedDir)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	// Collect valid worktrees for the reconciler's desired set.
	var validDirs []string
	for _, proj := range projects {
		// Filter bogus dirs: root dir, empty, or non-existent.
		if proj.Worktree == "" || proj.Worktree == "/" {
			continue
		}
		if _, err := os.Stat(proj.Worktree); os.IsNotExist(err) {
			continue
		}
		validDirs = append(validDirs, proj.Worktree)
	}

	// Add discovered worktrees to desired set so they get servers for
	// future backend operations. The reconciler will start them.
	if len(validDirs) > 0 {
		m.serverMgr.AddDesired(validDirs...)
	}

	// List sessions from the SEED server — all projects share the same
	// OpenCode database, so one server can return all sessions.
	sessions, err := m.serverMgr.ListSessionsFromServer(ctx, seedURL)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return sessions, nil
}

// ClaudeBackendManager implements agent.BackendManager for Claude Code sessions.
type ClaudeBackendManager struct{}

// NewClaudeBackendManager creates a new Claude backend manager.
func NewClaudeBackendManager() *ClaudeBackendManager {
	return &ClaudeBackendManager{}
}

// CreateBackend creates a Claude Code SessionBackend.
func (m *ClaudeBackendManager) CreateBackend(req agent.StartRequest) (agent.SessionBackend, error) {
	return agent.NewClaudeCodeBackend(), nil
}

// Init is a no-op for Claude — there are no long-lived servers to manage.
func (m *ClaudeBackendManager) Init(ctx context.Context, knownDirs func() ([]string, error)) error {
	return nil
}

// Shutdown is a no-op for Claude — each session manages its own SDK client connection.
func (m *ClaudeBackendManager) Shutdown() {}

// Daemon is the long-lived background process that manages agent sessions.
type Daemon struct {
	sockPath string
	pidPath  string
	listener net.Listener

	mu       sync.RWMutex
	sessions map[string]*managedSession // keyed by daemon session ID
	// subscribers receive all events broadcast by the daemon.
	subMu       sync.RWMutex
	subscribers map[string]chan agent.Event // keyed by subscriber ID

	startTime time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	// BackendManagers maps each backend type to its manager. The manager
	// creates SessionBackend instances and owns shared resources (e.g.,
	// OpenCode servers). Managers that also implement agent.AgentLister or
	// agent.SessionDiscoverer gain agent listing and session discovery
	// capabilities automatically.
	BackendManagers map[agent.BackendType]agent.BackendManager

	// Store is the optional SQLite persistence layer. When non-nil, session
	// metadata is written through on every mutation and loaded on startup.
	// When nil (e.g. in tests), the daemon operates purely in-memory.
	Store *store.Store

	// primaryAgentsRefreshMu guards primaryAgentsRefreshInFlight.
	primaryAgentsRefreshMu       sync.Mutex
	primaryAgentsRefreshInFlight map[string]bool // keyed by "backend\x00projectDir"

	// voice is the singleton voice session (nil when inactive).
	voice            *voice.Session
	voiceAudioConn   *websocket.Conn
	voiceAudioSource mindmouth.AudioSource
	voiceAudioSink   mindmouth.AudioSink

	log *log.Logger
}

// managedSession tracks a running agent session.
type managedSession struct {
	info      agent.SessionInfo
	backend   agent.SessionBackend // nil until started
	watchOnly bool                 // true when backend was started via Watch() (no prompt sent yet)
}

// New creates a new daemon instance. It does not start listening.
func New() (*Daemon, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, fmt.Errorf("config dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir config dir: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		sockPath:                     filepath.Join(dir, "daemon.sock"),
		pidPath:                      filepath.Join(dir, "daemon.pid"),
		sessions:                     make(map[string]*managedSession),
		subscribers:                  make(map[string]chan agent.Event),
		startTime:                    time.Now(),
		ctx:                          ctx,
		cancel:                       cancel,
		log:                          log.New(os.Stderr, "[clank-daemon] ", log.LstdFlags|log.Lmsgprefix),
		BackendManagers:              make(map[agent.BackendType]agent.BackendManager),
		primaryAgentsRefreshInFlight: make(map[string]bool),
	}, nil
}

// NewWithPaths creates a daemon with explicit socket and PID file paths.
// Used for testing where we don't want to use the default config directory.
func NewWithPaths(sockPath, pidPath string) *Daemon {
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		sockPath:                     sockPath,
		pidPath:                      pidPath,
		sessions:                     make(map[string]*managedSession),
		subscribers:                  make(map[string]chan agent.Event),
		startTime:                    time.Now(),
		ctx:                          ctx,
		cancel:                       cancel,
		log:                          log.New(os.Stderr, "[clank-daemon] ", log.LstdFlags|log.Lmsgprefix),
		BackendManagers:              make(map[agent.BackendType]agent.BackendManager),
		primaryAgentsRefreshInFlight: make(map[string]bool),
	}
}

// SocketPath returns the Unix socket path for the daemon.
func SocketPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.sock"), nil
}

// PIDPath returns the PID file path.
func PIDPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.pid"), nil
}

// IsRunning checks if a daemon is already running by reading the PID file
// and verifying the process exists.
func IsRunning() (bool, int, error) {
	pidPath, err := PIDPath()
	if err != nil {
		return false, 0, err
	}
	data, err := os.ReadFile(pidPath)
	if os.IsNotExist(err) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false, 0, nil // corrupt PID file, treat as not running
	}
	// Check if process exists by sending signal 0.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, pid, nil
	}
	err = proc.Signal(syscall.Signal(0))
	if err != nil {
		// Process doesn't exist, clean up stale PID file.
		os.Remove(pidPath)
		sockPath, _ := SocketPath()
		if sockPath != "" {
			os.Remove(sockPath)
		}
		return false, pid, nil
	}
	return true, pid, nil
}

// Run starts the daemon, listening on the Unix socket. It blocks until
// the context is cancelled or a termination signal is received.
func (d *Daemon) Run() error {
	// Clean up stale socket.
	os.Remove(d.sockPath)

	listener, err := net.Listen("unix", d.sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.sockPath, err)
	}
	d.listener = listener
	// Make socket accessible.
	if err := os.Chmod(d.sockPath, 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	// Write PID file.
	if err := os.WriteFile(d.pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		listener.Close()
		return fmt.Errorf("write PID file: %w", err)
	}

	d.log.Printf("daemon started (pid=%d, socket=%s)", os.Getpid(), d.sockPath)

	// Load persisted sessions from the store (if available).
	if d.Store != nil {
		sessions, err := d.Store.LoadSessions()
		if err != nil {
			d.log.Printf("warning: failed to load sessions from store: %v", err)
		} else {
			d.mu.Lock()
			for _, info := range sessions {
				// Sessions loaded from DB have no backend — they can't
				// be actively busy. Normalize stale statuses to idle so
				// the inbox doesn't show spinners or dead markers for
				// sessions interrupted by a daemon restart.
				if info.Status == agent.StatusBusy || info.Status == agent.StatusStarting || info.Status == agent.StatusDead {
					info.Status = agent.StatusIdle
				}
				d.sessions[info.ID] = &managedSession{info: info, backend: nil}
			}
			d.mu.Unlock()
			if len(sessions) > 0 {
				d.log.Printf("loaded %d sessions from store", len(sessions))
			}
		}
	}

	// Populate the desired server set from known project dirs and start
	// the reconciler loop. This is the ONLY mechanism that starts servers.
	// The first reconcile tick runs immediately, starting all known servers
	// in parallel.
	for bt, mgr := range d.BackendManagers {
		knownDirs := func() ([]string, error) {
			if d.Store == nil {
				return nil, nil
			}
			return d.Store.KnownProjectDirs(bt)
		}
		if err := mgr.Init(d.ctx, knownDirs); err != nil {
			d.log.Printf("warning: init %s backend: %v", bt, err)
		}
	}

	// Warm primary agent caches AFTER the reconciler has started. The
	// cache warmer uses GetOrStartServer which will wait for the reconciler
	// to finish starting servers rather than starting them itself.
	d.warmPrimaryAgentCaches()

	mux := http.NewServeMux()
	d.registerRoutes(mux)

	server := &http.Server{Handler: mux}

	// Handle termination signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP server in background.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			d.log.Printf("http serve error: %v", err)
		}
	}()

	// Wait for shutdown signal or context cancellation.
	select {
	case sig := <-sigCh:
		d.log.Printf("received signal %v, shutting down", sig)
	case <-d.ctx.Done():
		d.log.Printf("context cancelled, shutting down")
	}

	return d.shutdown(server)
}

// shutdown gracefully stops the daemon.
func (d *Daemon) shutdown(server *http.Server) error {
	d.cancel()

	// Stop the voice session if active.
	d.mu.Lock()
	if d.voice != nil {
		d.log.Println("stopping voice session")
		d.voice.Close()
		d.voice = nil
	}
	if d.voiceAudioConn != nil {
		d.voiceAudioConn.CloseNow()
		d.voiceAudioConn = nil
	}
	d.mu.Unlock()

	// Stop all managed sessions.
	d.mu.Lock()
	for id, ms := range d.sessions {
		if ms.backend != nil {
			d.log.Printf("stopping session %s", id)
			if err := ms.backend.Stop(); err != nil {
				d.log.Printf("error stopping session %s: %v", id, err)
			}
		}
	}
	d.mu.Unlock()

	// Shut down all backend managers (e.g., stop OpenCode servers).
	for bt, mgr := range d.BackendManagers {
		d.log.Printf("shutting down %s backend manager", bt)
		mgr.Shutdown()
	}

	// Close the persistence store.
	if d.Store != nil {
		if err := d.Store.Close(); err != nil {
			d.log.Printf("error closing store: %v", err)
		}
	}

	// Close all subscriber channels.
	d.subMu.Lock()
	for id, ch := range d.subscribers {
		close(ch)
		delete(d.subscribers, id)
	}
	d.subMu.Unlock()

	// Shutdown HTTP server.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		d.log.Printf("http shutdown error: %v", err)
	}

	// Clean up files.
	os.Remove(d.sockPath)
	os.Remove(d.pidPath)

	d.wg.Wait()
	d.log.Printf("daemon stopped")
	return nil
}

// Stop requests the daemon to shut down.
func (d *Daemon) Stop() {
	d.cancel()
}

// registerRoutes sets up the HTTP handlers on the mux.
func (d *Daemon) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ping", d.handlePing)
	mux.HandleFunc("POST /sessions", d.handleCreateSession)
	mux.HandleFunc("GET /sessions", d.handleListSessions)
	mux.HandleFunc("GET /sessions/search", d.handleSearchSessions)
	mux.HandleFunc("GET /sessions/{id}", d.handleGetSession)
	mux.HandleFunc("GET /sessions/{id}/messages", d.handleGetSessionMessages)
	mux.HandleFunc("POST /sessions/{id}/message", d.handleSendMessage)
	mux.HandleFunc("POST /sessions/{id}/revert", d.handleRevertSession)
	mux.HandleFunc("POST /sessions/{id}/abort", d.handleAbortSession)
	mux.HandleFunc("POST /sessions/{id}/read", d.handleMarkSessionRead)
	mux.HandleFunc("POST /sessions/{id}/followup", d.handleToggleFollowUp)
	mux.HandleFunc("POST /sessions/{id}/visibility", d.handleSetVisibility)
	mux.HandleFunc("POST /sessions/{id}/draft", d.handleSetDraft)
	mux.HandleFunc("DELETE /sessions/{id}", d.handleDeleteSession)
	mux.HandleFunc("GET /events", d.handleEvents)
	mux.HandleFunc("POST /permissions/{id}/reply", d.handlePermissionReply)
	mux.HandleFunc("GET /status", d.handleStatus)
	mux.HandleFunc("GET /agents", d.handleListAgents)
	mux.HandleFunc("POST /sessions/discover", d.handleDiscoverSessions)
	// Debug endpoints — backend-specific, not part of the general API.
	mux.HandleFunc("GET /debug/opencode/servers", d.handleDebugOpenCodeServers)

	// Voice endpoints.
	mux.HandleFunc("GET /voice/audio", d.handleVoiceAudio)
	mux.HandleFunc("POST /voice/start", d.handleVoiceStart)
	mux.HandleFunc("POST /voice/stop", d.handleVoiceStop)
	mux.HandleFunc("POST /voice/listen", d.handleVoiceListen)
	mux.HandleFunc("POST /voice/unlisten", d.handleVoiceUnlisten)
	mux.HandleFunc("GET /voice/status", d.handleVoiceStatus)
}

// --- HTTP Handlers ---

func (d *Daemon) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"pid":     os.Getpid(),
		"uptime":  time.Since(d.startTime).String(),
		"version": "0.1.0",
	})
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pid":      os.Getpid(),
		"uptime":   time.Since(d.startTime).String(),
		"sessions": d.snapshotSessions(),
	})
}

func (d *Daemon) handleListAgents(w http.ResponseWriter, r *http.Request) {
	backendStr := r.URL.Query().Get("backend")
	projectDir := r.URL.Query().Get("project_dir")

	if backendStr == "" || projectDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "backend and project_dir query params are required"})
		return
	}

	bt := agent.BackendType(backendStr)
	mgr, ok := d.BackendManagers[bt]
	if !ok {
		writeJSON(w, http.StatusOK, []agent.AgentInfo{})
		return
	}
	lister, ok := mgr.(agent.AgentLister)
	if !ok {
		writeJSON(w, http.StatusOK, []agent.AgentInfo{})
		return
	}

	// Try to serve from the persistent cache (SQLite) first. This avoids
	// blocking on the OpenCode server starting up — the response is instant.
	if d.Store != nil {
		cached, err := d.Store.LoadPrimaryAgents(bt, projectDir)
		if err != nil {
			d.log.Printf("warning: load cached primary agents: %v", err)
		}
		if cached != nil {
			// Return cached data immediately and refresh in the background.
			d.refreshPrimaryAgentsInBackground(bt, projectDir, lister)
			writeJSON(w, http.StatusOK, cached)
			return
		}
	}

	// No cache — must fetch synchronously (first time for this project).
	agents, err := lister.ListAgents(r.Context(), projectDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if agents == nil {
		agents = []agent.AgentInfo{}
	}
	d.persistPrimaryAgents(bt, projectDir, agents)
	writeJSON(w, http.StatusOK, agents)
}

// handleDebugOpenCodeServers returns running OpenCode server processes.
// This is a debug endpoint specific to the OpenCode backend — it type-asserts
// directly to *OpenCodeBackendManager rather than going through an interface.
func (d *Daemon) handleDebugOpenCodeServers(w http.ResponseWriter, r *http.Request) {
	type serverWithSessions struct {
		agent.ServerInfo
		SessionCount int `json:"session_count"`
	}

	ocMgr, ok := d.BackendManagers[agent.BackendOpenCode].(*OpenCodeBackendManager)
	if !ok {
		writeJSON(w, http.StatusOK, []serverWithSessions{})
		return
	}

	// Count sessions per project dir.
	d.mu.RLock()
	projectSessions := make(map[string]int)
	for _, ms := range d.sessions {
		projectSessions[ms.info.ProjectDir]++
	}
	d.mu.RUnlock()

	var result []serverWithSessions
	for _, srv := range ocMgr.ListServers() {
		result = append(result, serverWithSessions{
			ServerInfo:   srv,
			SessionCount: projectSessions[srv.ProjectDir],
		})
	}
	if result == nil {
		result = []serverWithSessions{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (d *Daemon) handleDiscoverSessions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProjectDir string `json:"project_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if body.ProjectDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project_dir is required"})
		return
	}
	// Try each backend manager that supports session discovery.
	var snapshots []agent.SessionSnapshot
	for _, mgr := range d.BackendManagers {
		discoverer, ok := mgr.(agent.SessionDiscoverer)
		if !ok {
			continue
		}
		found, err := discoverer.DiscoverSessions(r.Context(), body.ProjectDir)
		if err != nil {
			d.log.Printf("discover sessions: %v", err)
			continue // best-effort: try other managers
		}
		snapshots = append(snapshots, found...)
	}
	if snapshots == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"discovered": 0, "total": 0})
		return
	}

	// Register discovered sessions, skipping any whose ExternalID already exists
	// (i.e., sessions the daemon is already managing from a previous create or discover).
	// Also check backend.SessionID() to catch sessions whose Start() is still
	// in progress (ExternalID not yet written back to info).
	//
	// For duplicates, refresh backend-owned fields (title, timestamps) from the
	// snapshot while preserving user-owned fields (visibility, follow_up, draft,
	// last_read_at) from the in-memory/DB copy.
	added := 0
	d.mu.Lock()
	for _, snap := range snapshots {
		var existingMS *managedSession
		for _, existing := range d.sessions {
			if existing.info.ExternalID == snap.ID {
				existingMS = existing
				break
			}
			if existing.backend != nil && existing.backend.SessionID() == snap.ID {
				existingMS = existing
				break
			}
		}
		if existingMS != nil {
			// Refresh backend-owned fields from the snapshot.
			existingMS.info.Title = snap.Title
			existingMS.info.CreatedAt = snap.CreatedAt
			existingMS.info.UpdatedAt = snap.UpdatedAt
			existingMS.info.ProjectDir = snap.Directory
			existingMS.info.ProjectName = filepath.Base(snap.Directory)
			existingMS.info.RevertMessageID = snap.RevertMessageID
			// Normalize stale statuses for backend-less sessions —
			// same rationale as the startup normalization.
			if existingMS.backend == nil && (existingMS.info.Status == agent.StatusBusy || existingMS.info.Status == agent.StatusStarting || existingMS.info.Status == agent.StatusDead) {
				existingMS.info.Status = agent.StatusIdle
			}
			d.persistSession(existingMS)
			continue
		}
		id := ulid.Make().String()
		info := agent.SessionInfo{
			ID:              id,
			ExternalID:      snap.ID,
			Backend:         agent.BackendOpenCode,
			Status:          agent.StatusIdle,
			ProjectDir:      snap.Directory,
			ProjectName:     filepath.Base(snap.Directory),
			Title:           snap.Title,
			RevertMessageID: snap.RevertMessageID,
			CreatedAt:       snap.CreatedAt,
			UpdatedAt:       snap.UpdatedAt,
			LastReadAt:      snap.UpdatedAt, // Mark as read — they're not new activity
		}
		d.sessions[id] = &managedSession{info: info, backend: nil}
		d.persistSession(d.sessions[id])
		added++
	}
	d.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"discovered": added,
		"total":      len(snapshots),
	})
}

func (d *Daemon) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req agent.StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	info, err := d.createSession(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (d *Daemon) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.snapshotSessions())
}

func (d *Daemon) handleSearchSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	sinceRaw := r.URL.Query().Get("since")
	untilRaw := r.URL.Query().Get("until")
	visibility := agent.SessionVisibility(r.URL.Query().Get("visibility"))

	if q == "" && sinceRaw == "" && untilRaw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one of q, since, or until is required"})
		return
	}

	var p agent.SearchParams
	p.Query = q
	p.Visibility = visibility

	if sinceRaw != "" {
		t, err := parseTimeParam(sinceRaw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid since param: " + err.Error()})
			return
		}
		p.Since = t
	}
	if untilRaw != "" {
		t, err := parseTimeParam(untilRaw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid until param: " + err.Error()})
			return
		}
		p.Until = t
	}

	writeJSON(w, http.StatusOK, d.searchSessions(p))
}

func (d *Daemon) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.mu.RLock()
	ms, ok := d.sessions[id]
	d.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	info := ms.info
	if ms.backend != nil {
		info.Status = ms.backend.Status()
	}
	writeJSON(w, http.StatusOK, info)
}

// activateBackend creates and attaches a backend to a historical session
// (one loaded via discover that has backend == nil). The backend is started
// via Watch() to enable SSE streaming without sending a prompt. An event
// relay goroutine is started so that events from the backend flow through
// the daemon's broadcast system.
func (d *Daemon) activateBackend(id string, ms *managedSession) error {
	mgr, ok := d.BackendManagers[ms.info.Backend]
	if !ok {
		return fmt.Errorf("no backend manager registered for %s", ms.info.Backend)
	}
	backend, err := mgr.CreateBackend(agent.StartRequest{
		Backend:    ms.info.Backend,
		ProjectDir: ms.info.ProjectDir,
		SessionID:  ms.info.ExternalID,
	})
	if err != nil {
		return fmt.Errorf("activate backend: %w", err)
	}

	// Start watching for events (SSE) without sending a prompt.
	if err := backend.Watch(d.ctx); err != nil {
		return fmt.Errorf("watch backend: %w", err)
	}

	d.mu.Lock()
	ms.backend = backend
	ms.watchOnly = true
	d.mu.Unlock()

	// Start event relay goroutine so backend events flow through broadcast.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for evt := range backend.Events() {
			evt.SessionID = id
			d.broadcast(evt)

			if evt.Type == agent.EventStatusChange {
				if data, ok := evt.Data.(agent.StatusChangeData); ok {
					d.updateSessionStatus(id, data.NewStatus)
				}
			}
			if evt.Type == agent.EventTitleChange {
				if data, ok := evt.Data.(agent.TitleChangeData); ok {
					d.updateSessionTitle(id, data.Title)
				}
			}
			if evt.Type == agent.EventRevertChange {
				if data, ok := evt.Data.(agent.RevertChangeData); ok {
					d.updateSessionRevert(id, data.MessageID)
				}
			}
		}
	}()

	return nil
}

func (d *Daemon) handleGetSessionMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.mu.RLock()
	ms, ok := d.sessions[id]
	d.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if ms.backend == nil {
		// Historical session — activate a read-only backend to fetch messages.
		if err := d.activateBackend(id, ms); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	messages, err := ms.backend.Messages(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if messages == nil {
		messages = []agent.MessageData{}
	}
	writeJSON(w, http.StatusOK, messages)
}

func (d *Daemon) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Text  string `json:"text"`
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if body.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required"})
		return
	}

	d.mu.RLock()
	ms, ok := d.sessions[id]
	d.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}

	// Update the session's current agent if one was specified, clear any draft,
	// and reset visibility if the session was hidden (user re-engaging means
	// it's no longer done/archived).
	d.mu.Lock()
	if body.Agent != "" {
		ms.info.Agent = body.Agent
	}
	ms.info.Draft = ""
	if ms.info.Visibility == agent.VisibilityDone || ms.info.Visibility == agent.VisibilityArchived {
		ms.info.Visibility = agent.VisibilityVisible
	}
	d.persistSession(ms)
	d.mu.Unlock()

	if ms.backend == nil {
		// Historical session with no backend — create one and start it with
		// the follow-up prompt. Start() handles resume: it skips Session.New()
		// because sessionID is already set, starts SSE, then sends the prompt.
		mgr, ok := d.BackendManagers[ms.info.Backend]
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no backend manager for " + string(ms.info.Backend)})
			return
		}
		req := agent.StartRequest{
			Backend:    ms.info.Backend,
			ProjectDir: ms.info.ProjectDir,
			SessionID:  ms.info.ExternalID,
			Prompt:     body.Text,
			Agent:      body.Agent,
		}
		backend, err := mgr.CreateBackend(req)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		d.mu.Lock()
		ms.backend = backend
		ms.watchOnly = false
		d.mu.Unlock()

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.runBackend(id, ms, req)
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
		return
	}

	if ms.watchOnly {
		// Backend was started via Watch() (read-only observation). Try
		// SendMessage first — OpenCode supports this. If it fails (e.g.,
		// Claude), fall back to stopping the watch-only backend and starting
		// a fresh one via Start().
		d.mu.Lock()
		ms.watchOnly = false
		d.mu.Unlock()
	}

	opts := agent.SendMessageOpts{
		Text:  body.Text,
		Agent: body.Agent,
	}

	// Dispatch asynchronously — SendMessage blocks until the LLM responds.
	// The TUI tracks progress via the SSE event stream instead.
	go func() {
		if err := ms.backend.SendMessage(d.ctx, opts); err != nil {
			d.log.Printf("session %s: send message error: %v", id, err)
			d.broadcast(agent.Event{
				Type:      agent.EventError,
				SessionID: id,
				Timestamp: time.Now(),
				Data:      agent.ErrorData{Message: err.Error()},
			})
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

func (d *Daemon) handleAbortSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.mu.RLock()
	ms, ok := d.sessions[id]
	d.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if ms.backend == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session has no active backend"})
		return
	}

	if err := ms.backend.Abort(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

func (d *Daemon) handleRevertSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		MessageID string `json:"message_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if body.MessageID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message_id is required"})
		return
	}

	d.mu.RLock()
	ms, ok := d.sessions[id]
	d.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if ms.backend == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session has no active backend"})
		return
	}

	if err := ms.backend.Revert(r.Context(), body.MessageID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	d.mu.Lock()
	ms.info.RevertMessageID = body.MessageID
	d.persistSession(ms)
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "reverted"})
}

func (d *Daemon) handleMarkSessionRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.mu.Lock()
	ms, ok := d.sessions[id]
	if !ok {
		d.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	ms.info.LastReadAt = time.Now()
	d.persistSession(ms)
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (d *Daemon) handleToggleFollowUp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d.mu.Lock()
	ms, ok := d.sessions[id]
	if !ok {
		d.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	ms.info.FollowUp = !ms.info.FollowUp
	followUp := ms.info.FollowUp
	d.persistSession(ms)
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"follow_up": followUp})
}

func (d *Daemon) handleSetVisibility(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Visibility agent.SessionVisibility `json:"visibility"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	switch body.Visibility {
	case agent.VisibilityVisible, agent.VisibilityDone, agent.VisibilityArchived:
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid visibility: %q", body.Visibility)})
		return
	}

	d.mu.Lock()
	ms, ok := d.sessions[id]
	if !ok {
		d.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	ms.info.Visibility = body.Visibility
	d.persistSession(ms)
	d.mu.Unlock()
}

func (d *Daemon) handleSetDraft(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Draft string `json:"draft"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	d.mu.Lock()
	ms, ok := d.sessions[id]
	if !ok {
		d.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	ms.info.Draft = body.Draft
	d.persistSession(ms)
	d.mu.Unlock()
}

func (d *Daemon) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	d.mu.Lock()
	ms, ok := d.sessions[id]
	if !ok {
		d.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	delete(d.sessions, id)
	d.deletePersistedSession(id)
	d.mu.Unlock()

	if ms.backend != nil {
		ms.backend.Stop()
	}

	d.broadcast(agent.Event{
		Type:      agent.EventSessionDelete,
		SessionID: id,
		Timestamp: time.Now(),
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	subID := ulid.Make().String()
	ch := make(chan agent.Event, 64)

	d.subMu.Lock()
	d.subscribers[subID] = ch
	d.subMu.Unlock()

	defer func() {
		d.subMu.Lock()
		delete(d.subscribers, subID)
		d.subMu.Unlock()
	}()

	// Send initial connected event.
	writeSSE(w, "connected", map[string]string{"subscriber_id": subID})
	flusher.Flush()

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return // channel closed, daemon shutting down
			}
			writeSSE(w, string(evt.Type), evt)
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-d.ctx.Done():
			return
		}
	}
}

func (d *Daemon) handlePermissionReply(w http.ResponseWriter, r *http.Request) {
	// TODO: wire up when backends support permission handling
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not yet implemented"})
}

// --- Internal Methods ---

// snapshotSessions returns a point-in-time copy of all session infos
// with live status from backends.
func (d *Daemon) snapshotSessions() []agent.SessionInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	sessions := make([]agent.SessionInfo, 0, len(d.sessions))
	for _, ms := range d.sessions {
		info := ms.info
		if ms.backend != nil {
			info.Status = ms.backend.Status()
		}
		sessions = append(sessions, info)
	}
	return sessions
}

// searchSessions returns sessions matching the given search parameters.
//
// Query supports pipe-separated OR groups: "auth bug|dark mode" matches
// sessions containing ("auth" AND "bug") OR ("dark" AND "mode"). Matching
// is case-insensitive and word-boundary-aware (each term must appear at the
// start of a word) against the concatenation of title, prompt, draft, and
// project_name.
//
// Since/Until filter on UpdatedAt. Results are sorted by updated_at descending.
func (d *Daemon) searchSessions(p agent.SearchParams) []agent.SessionInfo {
	// Parse OR groups from the query. Each group is a slice of AND terms.
	var orGroups [][]string
	if p.Query != "" {
		for _, group := range strings.Split(p.Query, "|") {
			terms := strings.Fields(strings.ToLower(strings.TrimSpace(group)))
			if len(terms) > 0 {
				orGroups = append(orGroups, terms)
			}
		}
	}

	hasQuery := len(orGroups) > 0
	hasSince := !p.Since.IsZero()
	hasUntil := !p.Until.IsZero()

	all := d.snapshotSessions()
	results := make([]agent.SessionInfo, 0)
	for _, si := range all {
		// Visibility filter.
		switch p.Visibility {
		case agent.VisibilityAll:
			// No filter — include everything.
		case agent.VisibilityDone, agent.VisibilityArchived:
			if si.Visibility != p.Visibility {
				continue
			}
		default:
			// Default ("") — active sessions only.
			if si.Hidden() {
				continue
			}
		}

		// Time filter.
		if hasSince && si.UpdatedAt.Before(p.Since) {
			continue
		}
		if hasUntil && !si.UpdatedAt.Before(p.Until) {
			continue
		}

		// Text filter: match if ANY OR group matches (all terms in the group present).
		if hasQuery {
			hay := strings.ToLower(si.Title + " " + si.Prompt + " " + si.Draft + " " + si.ProjectName)
			matched := false
			for _, terms := range orGroups {
				allMatch := true
				for _, term := range terms {
					if !containsAtWordBoundary(hay, term) {
						allMatch = false
						break
					}
				}
				if allMatch {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		results = append(results, si)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].UpdatedAt.After(results[j].UpdatedAt)
	})

	return results
}

// containsAtWordBoundary reports whether term appears in hay starting at a
// word boundary — i.e. the match is at position 0 or preceded by a non-letter.
// Both hay and term must already be lowercased.
func containsAtWordBoundary(hay, term string) bool {
	start := 0
	for {
		i := strings.Index(hay[start:], term)
		if i < 0 {
			return false
		}
		pos := start + i
		if pos == 0 || !unicode.IsLetter(rune(hay[pos-1])) {
			return true
		}
		start = pos + 1
	}
}

// parseTimeParam parses a time parameter that is either an RFC 3339 timestamp
// or a relative duration suffix (e.g. "7d", "24h") interpreted as "ago from now".
// Supported units: h (hours), d (days).
func parseTimeParam(s string) (time.Time, error) {
	return agent.ParseTimeParam(s)
}

// createSession creates a new managed session and starts the backend.
func (d *Daemon) createSession(req agent.StartRequest) (*agent.SessionInfo, error) {
	mgr, ok := d.BackendManagers[req.Backend]
	if !ok {
		return nil, fmt.Errorf("no backend manager registered for %s", req.Backend)
	}
	backend, err := mgr.CreateBackend(req)
	if err != nil {
		return nil, fmt.Errorf("create backend: %w", err)
	}

	id := ulid.Make().String()
	now := time.Now()

	info := agent.SessionInfo{
		ID:          id,
		Backend:     req.Backend,
		Status:      agent.StatusStarting,
		ProjectDir:  req.ProjectDir,
		ProjectName: filepath.Base(req.ProjectDir),
		Prompt:      req.Prompt,
		TicketID:    req.TicketID,
		Agent:       req.Agent,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	ms := &managedSession{
		info:    info,
		backend: backend,
	}

	d.mu.Lock()
	d.sessions[id] = ms
	d.persistSession(ms)
	d.mu.Unlock()

	// Broadcast session creation.
	d.broadcast(agent.Event{
		Type:      agent.EventSessionCreate,
		SessionID: id,
		Timestamp: now,
		Data:      info,
	})

	// Start the backend in a goroutine.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.runBackend(id, ms, req)
	}()

	return &info, nil
}

// runBackend starts the backend and relays its events.
func (d *Daemon) runBackend(id string, ms *managedSession, req agent.StartRequest) {
	// Start relaying events BEFORE calling Start(), because Start() blocks
	// for the entire LLM response (Prompt() is synchronous). Events emitted
	// by the backend's SSE goroutine during Start() must be relayed in real time.
	events := ms.backend.Events()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for evt := range events {
			evt.SessionID = id
			d.broadcast(evt)

			if evt.Type == agent.EventStatusChange {
				if data, ok := evt.Data.(agent.StatusChangeData); ok {
					d.updateSessionStatus(id, data.NewStatus)
				}
			}
			if evt.Type == agent.EventTitleChange {
				if data, ok := evt.Data.(agent.TitleChangeData); ok {
					d.updateSessionTitle(id, data.Title)
				}
			}
			if evt.Type == agent.EventRevertChange {
				if data, ok := evt.Data.(agent.RevertChangeData); ok {
					d.updateSessionRevert(id, data.MessageID)
				}
			}
		}
	}()
	defer func() { <-done }() // wait for relay goroutine to finish

	if err := ms.backend.Start(d.ctx, req); err != nil {
		d.log.Printf("session %s: backend start error: %v", id, err)
		d.updateSessionStatus(id, agent.StatusError)
		d.broadcast(agent.Event{
			Type:      agent.EventError,
			SessionID: id,
			Timestamp: time.Now(),
			Data:      agent.ErrorData{Message: err.Error()},
		})
		return
	}

	// After Start() returns, capture the backend's native session ID so
	// future discover calls can deduplicate against it.
	if extID := ms.backend.SessionID(); extID != "" {
		d.mu.Lock()
		if ms2, ok := d.sessions[id]; ok {
			ms2.info.ExternalID = extID
			d.persistSession(ms2)
		}
		d.mu.Unlock()
	}

	// Backend event channel closed — mark as dead if still busy.
	d.mu.RLock()
	ms2, ok := d.sessions[id]
	d.mu.RUnlock()
	if ok && ms2.backend != nil {
		status := ms2.backend.Status()
		if status == agent.StatusBusy || status == agent.StatusStarting {
			d.updateSessionStatus(id, agent.StatusDead)
		}
	}
}

// updateSessionStatus updates the cached status and UpdatedAt.
func (d *Daemon) updateSessionStatus(id string, status agent.SessionStatus) {
	d.mu.Lock()
	if ms, ok := d.sessions[id]; ok {
		ms.info.Status = status
		ms.info.UpdatedAt = time.Now()
		d.persistSession(ms)
	}
	d.mu.Unlock()
}

// updateSessionTitle updates the cached title and UpdatedAt.
func (d *Daemon) updateSessionTitle(id string, title string) {
	d.mu.Lock()
	if ms, ok := d.sessions[id]; ok {
		ms.info.Title = title
		ms.info.UpdatedAt = time.Now()
		d.persistSession(ms)
	}
	d.mu.Unlock()
}

// updateSessionRevert updates the cached revert message ID.
func (d *Daemon) updateSessionRevert(id string, messageID string) {
	d.mu.Lock()
	if ms, ok := d.sessions[id]; ok {
		ms.info.RevertMessageID = messageID
		d.persistSession(ms)
	}
	d.mu.Unlock()
}

// persistSession writes the session to the store if persistence is enabled.
// Must be called while d.mu is held (read or write lock).
func (d *Daemon) persistSession(ms *managedSession) {
	if d.Store == nil {
		return
	}
	if err := d.Store.UpsertSession(ms.info); err != nil {
		d.log.Printf("warning: persist session %s: %v", ms.info.ID, err)
	}
}

// deletePersistedSession removes the session from the store if persistence is enabled.
func (d *Daemon) deletePersistedSession(id string) {
	if d.Store == nil {
		return
	}
	if err := d.Store.DeleteSession(id); err != nil {
		d.log.Printf("warning: delete persisted session %s: %v", id, err)
	}
}

// persistPrimaryAgents writes the primary agent list to the store for future cache hits.
func (d *Daemon) persistPrimaryAgents(bt agent.BackendType, projectDir string, agents []agent.AgentInfo) {
	if d.Store == nil {
		return
	}
	if err := d.Store.UpsertPrimaryAgents(bt, projectDir, agents); err != nil {
		d.log.Printf("warning: persist primary agents for %s/%s: %v", bt, projectDir, err)
	}
}

// refreshPrimaryAgentsInBackground kicks off an async refresh of the primary
// agent list for the given backend/project. The result is persisted to SQLite
// so that subsequent requests get the updated list. Safe to call multiple
// times — concurrent refreshes for the same key are deduplicated.
func (d *Daemon) refreshPrimaryAgentsInBackground(bt agent.BackendType, projectDir string, lister agent.AgentLister) {
	d.primaryAgentsRefreshMu.Lock()
	key := string(bt) + "\x00" + projectDir
	if d.primaryAgentsRefreshInFlight[key] {
		d.primaryAgentsRefreshMu.Unlock()
		return
	}
	d.primaryAgentsRefreshInFlight[key] = true
	d.primaryAgentsRefreshMu.Unlock()

	go func() {
		defer func() {
			d.primaryAgentsRefreshMu.Lock()
			delete(d.primaryAgentsRefreshInFlight, key)
			d.primaryAgentsRefreshMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer cancel()

		agents, err := lister.ListAgents(ctx, projectDir)
		if err != nil {
			d.log.Printf("background primary agent refresh for %s/%s: %v", bt, projectDir, err)
			return
		}
		if agents == nil {
			agents = []agent.AgentInfo{}
		}
		d.persistPrimaryAgents(bt, projectDir, agents)
	}()
}

// warmPrimaryAgentCaches fetches and persists primary agent lists for all
// known project directories. Called once on daemon startup after the
// reconciler has been started. The refreshPrimaryAgentsInBackground calls
// use GetOrStartServer which will wait for the reconciler to provide a
// running server — this method does NOT start servers itself.
func (d *Daemon) warmPrimaryAgentCaches() {
	if d.Store == nil {
		return
	}
	for bt, mgr := range d.BackendManagers {
		lister, ok := mgr.(agent.AgentLister)
		if !ok {
			continue
		}
		dirs, err := d.Store.KnownProjectDirs(bt)
		if err != nil {
			d.log.Printf("warning: load project dirs for %s: %v", bt, err)
			continue
		}
		for _, dir := range dirs {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				continue
			}
			d.refreshPrimaryAgentsInBackground(bt, dir, lister)
		}
	}
}

// broadcast sends an event to all connected subscribers.
func (d *Daemon) broadcast(evt agent.Event) {
	d.subMu.RLock()
	defer d.subMu.RUnlock()
	for _, ch := range d.subscribers {
		select {
		case ch <- evt:
		default:
			// Subscriber too slow, drop event to avoid blocking.
		}
	}
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeSSE(w io.Writer, event string, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, jsonData)
}
