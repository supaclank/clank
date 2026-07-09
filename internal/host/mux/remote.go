package hostmux

// HTTP routes for worktree↔GitHub-remote sync. Pure decode/dispatch/
// encode + error classification; orchestration lives in
// internal/host/remote_*.go.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

func (m *Mux) registerRemote(mx *http.ServeMux) {
	mx.HandleFunc("GET /worktrees/{id}/remote/status", m.handleRemoteStatus)
	mx.HandleFunc("POST /worktrees/{id}/remote/push", m.handleRemotePush)
	mx.HandleFunc("POST /worktrees/{id}/remote/pull", m.handleRemotePull)
	mx.HandleFunc("POST /worktrees/{id}/remote/resolve", m.handleRemoteResolve)
	mx.HandleFunc("POST /worktrees/{id}/remote/publish", m.handleRemotePublish)
}

func (m *Mux) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "worktree id is required"})
		return
	}
	result, err := m.svc.RemoteSyncStatus(r.Context(), id)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (m *Mux) handleRemotePush(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "worktree id is required"})
		return
	}
	result, err := m.svc.PushToRemote(r.Context(), id)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (m *Mux) handleRemotePull(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "worktree id is required"})
		return
	}
	result, err := m.svc.PullFromRemote(r.Context(), id)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (m *Mux) handleRemoteResolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "worktree id is required"})
		return
	}
	var req struct {
		Strategy host.ResolveStrategy `json:"strategy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "decode body: " + err.Error()})
		return
	}
	if req.Strategy == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "missing_field", Error: "strategy is required"})
		return
	}
	result, err := m.svc.ResolveRemote(r.Context(), id, req.Strategy)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (m *Mux) handleRemotePublish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "worktree id is required"})
		return
	}
	var req host.PublishRequest
	// Body carries optional name/private; an empty body is fine (EOF).
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "decode body: " + err.Error()})
		return
	}
	result, err := m.svc.PublishToRemote(r.Context(), id, req)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// writeRemoteError maps the remote-sync typed errors to HTTP statuses +
// stable machine codes, falling through to writeError for the shared
// host errors (ErrNotFound, ErrInvalidArgument, ...).
func writeRemoteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, host.ErrGitHubManagerUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, errResp{Code: "github_unavailable", Error: err.Error()})
	case errors.Is(err, host.ErrGitHubNotConnected):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_not_connected", Error: err.Error()})
	case errors.Is(err, host.ErrNoOriginRemote):
		writeJSON(w, http.StatusBadRequest, errResp{Code: "no_origin_remote", Error: err.Error()})
	case errors.Is(err, host.ErrDetachedHead):
		writeJSON(w, http.StatusConflict, errResp{Code: "detached_head", Error: err.Error()})
	case errors.Is(err, host.ErrNoUpstream):
		writeJSON(w, http.StatusBadRequest, errResp{Code: "no_upstream", Error: err.Error()})
	case errors.Is(err, host.ErrWorktreeDirty):
		writeJSON(w, http.StatusConflict, errResp{Code: "worktree_dirty", Error: err.Error()})
	case errors.Is(err, host.ErrRemoteDiverged):
		writeJSON(w, http.StatusConflict, errResp{Code: "remote_diverged", Error: err.Error()})
	case errors.Is(err, host.ErrNotMerging):
		writeJSON(w, http.StatusConflict, errResp{Code: "not_merging", Error: err.Error()})
	case errors.Is(err, host.ErrNoCommonAncestor):
		writeJSON(w, http.StatusConflict, errResp{Code: "no_common_ancestor", Error: err.Error()})
	case errors.Is(err, host.ErrBaseRefUnreachable):
		writeJSON(w, http.StatusBadGateway, errResp{Code: "base_ref_unreachable", Error: err.Error()})
	case errors.Is(err, git.ErrPushRepoNotFound):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_repo_not_accessible", Error: err.Error()})
	case errors.Is(err, git.ErrPushPermissionDenied):
		writeJSON(w, http.StatusForbidden, errResp{Code: "push_denied", Error: err.Error()})
	case errors.Is(err, host.ErrAlreadyPublished):
		writeJSON(w, http.StatusConflict, errResp{Code: "already_published", Error: err.Error()})
	case errors.Is(err, host.ErrInvalidRepoName):
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_repo_name", Error: err.Error()})
	case errors.Is(err, githubpkg.ErrRepoNameTaken):
		writeJSON(w, http.StatusConflict, errResp{Code: "repo_name_taken", Error: err.Error()})
	default:
		writeError(w, err)
	}
}
