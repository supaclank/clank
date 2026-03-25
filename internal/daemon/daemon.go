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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	"github.com/oklog/ulid/v2"
)

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

	// BackendFactory creates a Backend for a given BackendType.
	// Defaults to returning an error (no backends registered).
	// Set by the caller before Run() to wire in real backends.
	BackendFactory func(agent.BackendType) (agent.Backend, error)

	log *log.Logger
}

// managedSession tracks a running agent session.
type managedSession struct {
	info    agent.SessionInfo
	backend agent.Backend // nil until started
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
		sockPath:    filepath.Join(dir, "daemon.sock"),
		pidPath:     filepath.Join(dir, "daemon.pid"),
		sessions:    make(map[string]*managedSession),
		subscribers: make(map[string]chan agent.Event),
		startTime:   time.Now(),
		ctx:         ctx,
		cancel:      cancel,
		log:         log.New(os.Stderr, "[clank-daemon] ", log.LstdFlags|log.Lmsgprefix),
		BackendFactory: func(bt agent.BackendType) (agent.Backend, error) {
			return nil, fmt.Errorf("no backend registered for %s", bt)
		},
	}, nil
}

// NewWithPaths creates a daemon with explicit socket and PID file paths.
// Used for testing where we don't want to use the default config directory.
func NewWithPaths(sockPath, pidPath string) *Daemon {
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		sockPath:    sockPath,
		pidPath:     pidPath,
		sessions:    make(map[string]*managedSession),
		subscribers: make(map[string]chan agent.Event),
		startTime:   time.Now(),
		ctx:         ctx,
		cancel:      cancel,
		log:         log.New(os.Stderr, "[clank-daemon] ", log.LstdFlags|log.Lmsgprefix),
		BackendFactory: func(bt agent.BackendType) (agent.Backend, error) {
			return nil, fmt.Errorf("no backend registered for %s", bt)
		},
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
	mux.HandleFunc("GET /sessions/{id}", d.handleGetSession)
	mux.HandleFunc("POST /sessions/{id}/message", d.handleSendMessage)
	mux.HandleFunc("POST /sessions/{id}/abort", d.handleAbortSession)
	mux.HandleFunc("DELETE /sessions/{id}", d.handleDeleteSession)
	mux.HandleFunc("GET /events", d.handleEvents)
	mux.HandleFunc("POST /permissions/{id}/reply", d.handlePermissionReply)
	mux.HandleFunc("GET /status", d.handleStatus)
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
	d.mu.RLock()
	sessions := make([]agent.SessionInfo, 0, len(d.sessions))
	for _, ms := range d.sessions {
		info := ms.info
		if ms.backend != nil {
			info.Status = ms.backend.Status()
		}
		sessions = append(sessions, info)
	}
	d.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pid":      os.Getpid(),
		"uptime":   time.Since(d.startTime).String(),
		"sessions": sessions,
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
	d.mu.RLock()
	sessions := make([]agent.SessionInfo, 0, len(d.sessions))
	for _, ms := range d.sessions {
		info := ms.info
		if ms.backend != nil {
			info.Status = ms.backend.Status()
		}
		sessions = append(sessions, info)
	}
	d.mu.RUnlock()

	writeJSON(w, http.StatusOK, sessions)
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

func (d *Daemon) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Text string `json:"text"`
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
	if ms.backend == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session has no active backend"})
		return
	}

	if err := ms.backend.SendMessage(r.Context(), body.Text); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
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

// createSession creates a new managed session and starts the backend.
func (d *Daemon) createSession(req agent.StartRequest) (*agent.SessionInfo, error) {
	backend, err := d.BackendFactory(req.Backend)
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
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	ms := &managedSession{
		info:    info,
		backend: backend,
	}

	d.mu.Lock()
	d.sessions[id] = ms
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

	// Relay events from the backend to all subscribers.
	events := ms.backend.Events()
	if events == nil {
		return
	}
	for evt := range events {
		evt.SessionID = id // ensure daemon ID is set
		d.broadcast(evt)

		// Update session status if it's a status change event.
		if evt.Type == agent.EventStatusChange {
			if data, ok := evt.Data.(agent.StatusChangeData); ok {
				d.updateSessionStatus(id, data.NewStatus)
			}
		}
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
	}
	d.mu.Unlock()
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
