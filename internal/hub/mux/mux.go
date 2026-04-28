// Package hubmux exposes a *hub.Service over HTTP. It is consumed by
// `cmd/clankd` which serves it on a Unix socket.
//
// Mux owns request decoding, response encoding, and HTTP status codes.
// All actual logic lives on *hub.Service in internal/hub/api.go.
//
// Symmetric with internal/host/mux. Step 2 of the hub-host refactor
// (see hub_host_refactor_code_review.md §7.8) extracted handlers off
// *hub.Service into this package.
package hubmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/acksell/clank/internal/hub"
	clanksync "github.com/acksell/clank/internal/sync"
)

// Mux wraps a *hub.Service with an http.Handler.
type Mux struct {
	svc  *hub.Service
	log  *log.Logger
	sync *clanksync.Receiver // nil = /sync/* endpoints disabled
}

// New constructs a Mux. log may be nil.
func New(svc *hub.Service, lg *log.Logger) *Mux {
	if svc == nil {
		panic("hubmux.New: svc is required")
	}
	if lg == nil {
		lg = log.Default()
	}
	return &Mux{svc: svc, log: lg}
}

// WithSync attaches a sync.Receiver. The hub-to-hub /sync/* endpoints
// are registered only when one is set; tests that don't exercise sync
// can skip this.
func (m *Mux) WithSync(r *clanksync.Receiver) *Mux {
	m.sync = r
	return m
}

// Handler returns an http.Handler with all routes registered.
func (m *Mux) Handler() http.Handler {
	mx := http.NewServeMux()
	m.register(mx)
	return mx
}

func (m *Mux) register(mx *http.ServeMux) {
	// Liveness / status / event stream.
	mx.HandleFunc("GET /ping", m.handlePing)
	mx.HandleFunc("GET /status", m.handleStatus)
	mx.HandleFunc("GET /events", m.handleEvents)

	// Sessions.
	mx.HandleFunc("POST /sessions", m.handleCreateSession)
	mx.HandleFunc("GET /sessions", m.handleListSessions)
	mx.HandleFunc("GET /sessions/search", m.handleSearchSessions)
	mx.HandleFunc("GET /sessions/{id}", m.handleGetSession)
	mx.HandleFunc("GET /sessions/{id}/messages", m.handleGetSessionMessages)
	mx.HandleFunc("POST /sessions/{id}/message", m.handleSendMessage)
	mx.HandleFunc("POST /sessions/{id}/revert", m.handleRevertSession)
	mx.HandleFunc("POST /sessions/{id}/fork", m.handleForkSession)
	mx.HandleFunc("POST /sessions/{id}/abort", m.handleAbortSession)
	mx.HandleFunc("POST /sessions/{id}/read", m.handleMarkSessionRead)
	mx.HandleFunc("POST /sessions/{id}/followup", m.handleToggleFollowUp)
	mx.HandleFunc("POST /sessions/{id}/visibility", m.handleSetVisibility)
	mx.HandleFunc("POST /sessions/{id}/draft", m.handleSetDraft)
	mx.HandleFunc("DELETE /sessions/{id}", m.handleDeleteSession)
	mx.HandleFunc("POST /sessions/discover", m.handleDiscoverSessions)

	// Permissions.
	mx.HandleFunc("POST /sessions/{id}/permissions/{permID}/reply", m.handlePermissionReply)
	mx.HandleFunc("GET /sessions/{id}/pending-permission", m.handleGetPendingPermission)

	// Agents / models.
	mx.HandleFunc("GET /agents", m.handleListAgents)
	mx.HandleFunc("GET /models", m.handleListModels)

	// Hosts / worktrees. Identity (`GitRef`, branch) is in the request
	// body — branch names contain "/" so they cannot ride in the path,
	// and there is no host-side repo registry to bind path params to
	// post-§7.
	mx.HandleFunc("POST /hosts/{hostname}/worktrees/list-branches", m.handleListBranchesOnHost)
	mx.HandleFunc("POST /hosts/{hostname}/worktrees/resolve", m.handleResolveWorktreeOnHost)
	mx.HandleFunc("POST /hosts/{hostname}/worktrees/remove", m.handleRemoveWorktreeOnHost)
	mx.HandleFunc("POST /hosts/{hostname}/worktrees/merge", m.handleMergeBranchOnHost)

	// Voice. The websocket handler stays on *hub.Service because it owns
	// the voice singleton state and the long-lived ws connection; mux
	// only delegates.
	mx.HandleFunc("GET /voice/audio", m.svc.HandleVoiceAudio)
	mx.HandleFunc("GET /voice/status", m.handleVoiceStatus)

	// Hub-to-hub sync. Registered only when a sync.Receiver is wired up
	// (cloud-hub mode) — the laptop hub does not expose these. See
	// internal/hub/mux/sync.go.
	if m.sync != nil {
		m.registerSync(mx)
	}
}

// --- helpers ---

func (m *Mux) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Status/headers already flushed; we can't change the response
		// shape, but log so failures are not invisible.
		m.log.Printf("hubmux: writeJSON encode error: %v", err)
	}
}

// writeJSON is the package-level shim used by handlers that don't have
// access to the Mux receiver yet. Prefer (*Mux).writeJSON for new
// handlers so encode errors get logged with the daemon logger.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("hubmux: writeJSON encode error: %v", err)
	}
}

// writeSSE writes a single Server-Sent Events frame. Returns the first
// non-nil error from json.Marshal or the underlying writer so callers
// can decide whether to abort the stream (typical: client disconnected).
func writeSSE(w io.Writer, event string, data interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal SSE %q payload: %w", event, err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, jsonData); err != nil {
		return fmt.Errorf("write SSE %q frame: %w", event, err)
	}
	return nil
}

func writeBadRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

func writeInternal(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// writeServiceErr maps Service-layer sentinel errors to HTTP statuses.
// Anything not recognized falls through to 500 with the err.Error()
// body. Status-shape parity with the legacy handlers is preserved by
// the per-endpoint mux files when the legacy code used a more specific
// status (e.g., 409 Conflict for "no active backend").
func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hub.ErrSessionNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, hub.ErrNoActiveBackend):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, hub.ErrInvalidVisibility), errors.Is(err, hub.ErrInvalidRequest):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		var hostErr hub.ErrHostNotRegisteredErr
		if errors.As(err, &hostErr) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeInternal(w, err)
	}
}

func decodeJSON(r io.Reader, v interface{}) error {
	return json.NewDecoder(r).Decode(v)
}
