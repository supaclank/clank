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
	svc       *host.Service
	log       *log.Logger
	authToken string

	// builds tracks in-progress pull-back checkpoint builds keyed by
	// build_id. See internal/host/mux/sync.go for the three-step flow
	// (build → upload → delete) that uses this.
	builds *spriteBuildStore
}

// New constructs a Mux. log may be nil.
func New(svc *host.Service, lg *log.Logger) *Mux {
	if svc == nil {
		panic("hostmux.New: svc is required")
	}
	if lg == nil {
		lg = log.Default()
	}
	return &Mux{svc: svc, log: lg, builds: newSpriteBuildStore()}
}

// SetAuthToken configures the bearer-token middleware. When non-empty,
// every request must carry "Authorization: Bearer <token>" or the
// handler returns 401. When empty (the default), no auth is enforced
// — that's the laptop-local single-user mode where a Unix socket or
// loopback TCP listener is the only access boundary.
//
// Must be called before Handler() so the wrap is applied; calling
// after has no effect on the already-built handler.
func (m *Mux) SetAuthToken(token string) {
	m.authToken = token
}

// Handler returns an http.Handler with all routes registered. When an
// auth token is configured via SetAuthToken, the entire handler is
// wrapped in the requireBearer middleware so every endpoint is
// gated uniformly.
func (m *Mux) Handler() http.Handler {
	mx := http.NewServeMux()
	m.register(mx)
	return requireBearer(m.authToken)(mx)
}

func (m *Mux) register(mx *http.ServeMux) {
	mx.HandleFunc("GET /status", m.handleStatus)
	mx.HandleFunc("GET /ping", m.handlePing)
	// PR 3: global SSE event stream. TUI/mobile subscribers attach
	// here through the gateway. Per-session SSE at
	// /sessions/{id}/events stays for host-client back-compat.
	mx.HandleFunc("GET /events", m.handleEvents)
	mx.HandleFunc("GET /backends", m.handleListBackends)
	mx.HandleFunc("GET /agents", m.handleListAgents)
	mx.HandleFunc("GET /models", m.handleListModels)
	// /discover is the legacy host-client path; /sessions/discover
	// is the TUI-facing path the gateway routes from.
	mx.HandleFunc("POST /discover", m.handleDiscoverSessions)
	mx.HandleFunc("POST /sessions/discover", m.handleDiscoverSessions)

	// Worktree/branch ops. The repo is identified by GitRef in the
	// request body — the host repo registry was removed in §7.8.
	mx.HandleFunc("POST /worktrees/list-branches", m.handleListBranches)
	mx.HandleFunc("POST /worktrees/resolve", m.handleResolveWorktree)
	mx.HandleFunc("POST /worktrees/remove", m.handleRemoveWorktree)
	mx.HandleFunc("POST /worktrees/merge", m.handleMergeBranch)

	mx.HandleFunc("POST /sessions", m.handleCreateSession)
	mx.HandleFunc("GET /sessions", m.handleListSessions)
	mx.HandleFunc("GET /sessions/search", m.handleSearchSessions)
	mx.HandleFunc("POST /sessions/{id}/open", m.handleOpenSession)
	// /sessions/{id}/message is the TUI-facing path; /send is the
	// legacy host-client path. Both registered until the hub goes.
	mx.HandleFunc("POST /sessions/{id}/message", m.handleSendSession)
	mx.HandleFunc("POST /sessions/{id}/send", m.handleSendSession)
	mx.HandleFunc("POST /sessions/{id}/open-and-send", m.handleOpenAndSendSession)
	mx.HandleFunc("POST /sessions/{id}/abort", m.handleAbortSession)
	mx.HandleFunc("POST /sessions/{id}/revert", m.handleRevertSession)
	mx.HandleFunc("POST /sessions/{id}/fork", m.handleForkSession)
	mx.HandleFunc("POST /sessions/{id}/read", m.handleMarkSessionRead)
	mx.HandleFunc("POST /sessions/{id}/followup", m.handleToggleSessionFollowUp)
	mx.HandleFunc("POST /sessions/{id}/visibility", m.handleSetSessionVisibility)
	mx.HandleFunc("POST /sessions/{id}/draft", m.handleSetSessionDraft)
	mx.HandleFunc("DELETE /sessions/{id}", m.handleDeleteSession)
	mx.HandleFunc("GET /sessions/{id}/messages", m.handleGetMessages)
	mx.HandleFunc("GET /sessions/{id}/events", m.handleSessionEvents)
	mx.HandleFunc("GET /sessions/{id}/pending-permission", m.handlePendingPermissions)
	mx.HandleFunc("POST /sessions/{id}/permissions/{permID}/reply", m.handlePermissionReply)
	mx.HandleFunc("POST /sessions/{id}/stop", m.handleStopSession)
	mx.HandleFunc("GET /sessions/{id}", m.handleGetSession)

	// Provider authentication. See internal/host/mux/auth.go.
	m.registerAuth(mx)

	// Cloud-sync ingress. The gateway orchestrates pushes and pulls
	// through these endpoints; the sandbox is a pure responder.
	//
	//   - POST /sync/apply-from-urls    — apply a checkpoint by pulling presigned
	//                                     GET URLs the gateway minted. Push path.
	//   - POST /sync/build?repo=<id>    — pull-back step 1: build bundles to local
	//                                     disk, return metadata + build_id.
	//   - POST /sync/builds/{id}/upload — pull-back step 2: PUT bundles to the
	//                                     presigned URLs in the request body.
	//   - DELETE /sync/builds/{id}      — pull-back step 3 (idempotent cleanup).
	// See sync.go.
	mx.HandleFunc("POST /sync/apply-from-urls", m.handleSyncApplyFromURLs)
	mx.HandleFunc("POST /sync/build", m.handleSyncBuild)
	mx.HandleFunc("POST /sync/builds/{id}/upload", m.handleSyncBuildsUpload)
	mx.HandleFunc("DELETE /sync/builds/{id}", m.handleSyncBuildsDelete)
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
