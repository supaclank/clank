package hostmux

// GitRef-addressed variants of the remote-sync and PR routes. The
// {id}-keyed routes in remote.go / github_pr.go serve gateway clients
// that hold a managed worktree id; these serve callers that only know
// a git ref — the local `clank preview` overlay, whose project is an
// arbitrary local checkout (git_ref.local_path), mirroring the
// existing POST /worktrees/list-branches addressing style.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
)

func (m *Mux) registerRemoteRef(mx *http.ServeMux) {
	mx.HandleFunc("POST /worktrees/remote-status", m.handleRemoteStatusRef)
	mx.HandleFunc("POST /worktrees/remote-push", m.handleRemotePushRef)
	mx.HandleFunc("POST /worktrees/remote-pull", m.handleRemotePullRef)
	mx.HandleFunc("POST /worktrees/remote-resolve", m.handleRemoteResolveRef)
	mx.HandleFunc("POST /worktrees/remote-publish", m.handleRemotePublishRef)
	mx.HandleFunc("POST /worktrees/create-pr", m.handleCreatePRRef)
	mx.HandleFunc("POST /worktrees/pr-ready", m.handleMarkPRReadyRef)
}

// maxRefBody caps inbound body size for GitRef-addressed routes — these
// serve browser callers, so an unbounded decode is a resource-exhaustion
// vector.
const maxRefBody = 1 << 20

// decodeRefBody decodes a JSON body into dst and validates the GitRef
// returned by refOf. Writes the 400 itself and returns false on failure.
func decodeRefBody[T any](w http.ResponseWriter, r *http.Request, dst *T, refOf func(*T) agent.GitRef) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRefBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errResp{Code: "request_too_large", Error: err.Error()})
			return false
		}
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "decode body: " + err.Error()})
		return false
	}
	if err := refOf(dst).Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: err.Error()})
		return false
	}
	return true
}

type remoteRefRequest struct {
	GitRef agent.GitRef `json:"git_ref"`
}

func (m *Mux) handleRemoteStatusRef(w http.ResponseWriter, r *http.Request) {
	var req remoteRefRequest
	if !decodeRefBody(w, r, &req, func(q *remoteRefRequest) agent.GitRef { return q.GitRef }) {
		return
	}
	result, err := m.svc.RemoteSyncStatus(r.Context(), req.GitRef)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (m *Mux) handleRemotePushRef(w http.ResponseWriter, r *http.Request) {
	var req remoteRefRequest
	if !decodeRefBody(w, r, &req, func(q *remoteRefRequest) agent.GitRef { return q.GitRef }) {
		return
	}
	result, err := m.svc.PushToRemote(r.Context(), req.GitRef)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (m *Mux) handleRemotePullRef(w http.ResponseWriter, r *http.Request) {
	var req remoteRefRequest
	if !decodeRefBody(w, r, &req, func(q *remoteRefRequest) agent.GitRef { return q.GitRef }) {
		return
	}
	result, err := m.svc.PullFromRemote(r.Context(), req.GitRef)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type remoteResolveRefRequest struct {
	GitRef   agent.GitRef         `json:"git_ref"`
	Strategy host.ResolveStrategy `json:"strategy"`
}

func (m *Mux) handleRemoteResolveRef(w http.ResponseWriter, r *http.Request) {
	var req remoteResolveRefRequest
	if !decodeRefBody(w, r, &req, func(q *remoteResolveRefRequest) agent.GitRef { return q.GitRef }) {
		return
	}
	if req.Strategy == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "missing_field", Error: "strategy is required"})
		return
	}
	result, err := m.svc.ResolveRemote(r.Context(), req.GitRef, req.Strategy)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type remotePublishRefRequest struct {
	GitRef agent.GitRef `json:"git_ref"`
	host.PublishRequest
}

func (m *Mux) handleRemotePublishRef(w http.ResponseWriter, r *http.Request) {
	var req remotePublishRefRequest
	if !decodeRefBody(w, r, &req, func(q *remotePublishRefRequest) agent.GitRef { return q.GitRef }) {
		return
	}
	result, err := m.svc.PublishToRemote(r.Context(), req.GitRef, req.PublishRequest)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type createPRRefRequest struct {
	GitRef agent.GitRef `json:"git_ref"`
	host.CreatePRRequest
}

func (m *Mux) handleCreatePRRef(w http.ResponseWriter, r *http.Request) {
	var req createPRRefRequest
	if !decodeRefBody(w, r, &req, func(q *createPRRefRequest) agent.GitRef { return q.GitRef }) {
		return
	}
	result, err := m.svc.CreatePR(r.Context(), req.GitRef, req.CreatePRRequest)
	if err != nil {
		writePRError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (m *Mux) handleMarkPRReadyRef(w http.ResponseWriter, r *http.Request) {
	var req remoteRefRequest
	if !decodeRefBody(w, r, &req, func(q *remoteRefRequest) agent.GitRef { return q.GitRef }) {
		return
	}
	result, err := m.svc.MarkPRReady(r.Context(), req.GitRef)
	if err != nil {
		writePRError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
