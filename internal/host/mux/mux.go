// Package hostmux exposes a *host.Service over HTTP. It is consumed by
// `cmd/clank-host` (which serves it on a Unix socket) and by tests that
// want to exercise the wire format.
//
// The Hub never imports this package directly; it goes through
// internal/host/client (hostclient) which has both an in-process adapter
// (calls *host.Service methods) and an HTTP adapter (calls these
// handlers). The wire endpoints are designed to be the only contract
// between Hub and Host.
package hostmux

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/acksell/clank/internal/host"
)

// Mux wraps a *host.Service with an http.Handler.
type Mux struct {
	svc *host.Service
	log *log.Logger
}

// New constructs a Mux. log may be nil.
func New(svc *host.Service, lg *log.Logger) *Mux {
	if svc == nil {
		panic("hostmux.New: svc is required")
	}
	if lg == nil {
		lg = log.Default()
	}
	return &Mux{svc: svc, log: lg}
}

// Handler returns an http.Handler with all routes registered.
func (m *Mux) Handler() http.Handler {
	mx := http.NewServeMux()
	m.register(mx)
	return mx
}

func (m *Mux) register(mx *http.ServeMux) {
	mx.HandleFunc("GET /status", m.handleStatus)
	mx.HandleFunc("GET /backends", m.handleListBackends)
	mx.HandleFunc("GET /agents", m.handleListAgents)
	mx.HandleFunc("GET /models", m.handleListModels)
	mx.HandleFunc("POST /discover", m.handleDiscoverSessions)

	// Worktree/branch ops. The repo is identified by GitRef in the
	// request body — the host repo registry was removed in §7.8.
	mx.HandleFunc("POST /worktrees/list-branches", m.handleListBranches)
	mx.HandleFunc("POST /worktrees/resolve", m.handleResolveWorktree)
	mx.HandleFunc("POST /worktrees/remove", m.handleRemoveWorktree)
	mx.HandleFunc("POST /worktrees/merge", m.handleMergeBranch)

	mx.HandleFunc("POST /sessions", m.handleCreateSession)
	mx.HandleFunc("POST /sessions/{id}/start", m.handleStartSession)
	mx.HandleFunc("POST /sessions/{id}/watch", m.handleWatchSession)
	mx.HandleFunc("POST /sessions/{id}/message", m.handleSendMessage)
	mx.HandleFunc("POST /sessions/{id}/abort", m.handleAbortSession)
	mx.HandleFunc("POST /sessions/{id}/revert", m.handleRevertSession)
	mx.HandleFunc("POST /sessions/{id}/fork", m.handleForkSession)
	mx.HandleFunc("GET /sessions/{id}/messages", m.handleGetMessages)
	mx.HandleFunc("GET /sessions/{id}/events", m.handleSessionEvents)
	mx.HandleFunc("POST /sessions/{id}/permissions/{permID}/reply", m.handlePermissionReply)
	mx.HandleFunc("POST /sessions/{id}/stop", m.handleStopSession)
	mx.HandleFunc("GET /sessions/{id}", m.handleSessionSnapshot)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, host.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errResp{Code: "not_found", Error: err.Error()})
	case errors.Is(err, host.ErrCannotMergeDefault):
		writeJSON(w, http.StatusConflict, errResp{Code: "cannot_merge_default", Error: err.Error()})
	case errors.Is(err, host.ErrNothingToMerge):
		writeJSON(w, http.StatusConflict, errResp{Code: "nothing_to_merge", Error: err.Error()})
	case errors.Is(err, host.ErrCommitMessageRequired):
		writeJSON(w, http.StatusConflict, errResp{Code: "commit_message_required", Error: err.Error()})
	case errors.Is(err, host.ErrTargetDirty):
		writeJSON(w, http.StatusConflict, errResp{Code: "main_dirty", Error: err.Error()})
	case errors.Is(err, host.ErrMergeConflict):
		writeJSON(w, http.StatusConflict, errResp{Code: "merge_conflict", Error: err.Error()})
	case errors.Is(err, host.ErrReservedBranch):
		writeJSON(w, http.StatusConflict, errResp{Code: "reserved_branch", Error: err.Error()})
	case errors.Is(err, host.ErrInvalidBranchName):
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_branch_name", Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, errResp{Code: "internal", Error: err.Error()})
	}
}

// errResp is the wire format for error responses. Code is a stable
// machine-readable identifier the client maps back to a sentinel error
// (see hostclient/http.go: errorFromResp).
type errResp struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func decodeJSON(r io.Reader, v interface{}) error {
	return json.NewDecoder(r).Decode(v)
}
