package hostmux

// HTTP routes for the repo-first surface (/repos/...). Pure decode/
// dispatch/encode; orchestration lives in internal/host/repos*.go. The
// list/overview/delete routes join in the next phase.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/acksell/clank/internal/host"
)

func (m *Mux) registerRepos(mx *http.ServeMux) {
	mx.HandleFunc("POST /repos/{slug}/worktrees", m.handleCreateRepoWorktree)
}

// handleCreateRepoWorktree services POST /repos/{slug}/worktrees — load
// an existing branch into a worktree ({"branch": …}, idempotent) or
// fork a fresh petname branch off a base ({"base_branch": …}). 201 on a
// newly created worktree, 200 when an already-loaded branch's worktree
// is returned.
func (m *Mux) handleCreateRepoWorktree(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "repo slug is required"})
		return
	}
	var req host.RepoWorktreeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "decode body: " + err.Error()})
		return
	}
	result, err := m.svc.CreateRepoWorktree(r.Context(), slug, req)
	if err != nil {
		writeRepoError(w, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

// writeRepoError maps repo-scoped typed errors to statuses + stable
// machine codes, falling through to writeError for the shared host
// errors (ErrNotFound, ErrInvalidArgument, …).
func writeRepoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, host.ErrRepoNotFound):
		writeJSON(w, http.StatusNotFound, errResp{Code: "repo_not_found", Error: err.Error()})
	case errors.Is(err, host.ErrGitHubManagerUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, errResp{Code: "github_unavailable", Error: err.Error()})
	case errors.Is(err, host.ErrGitHubNotConnected):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_not_connected", Error: err.Error()})
	default:
		writeError(w, err)
	}
}
