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
	mx.HandleFunc("GET /repos", m.handleListRepos)
	mx.HandleFunc("POST /repos/{slug}/worktrees", m.handleCreateRepoWorktree)
	mx.HandleFunc("GET /repos/{slug}/overview", m.handleRepoOverview)
	mx.HandleFunc("DELETE /repos/{slug}", m.handleDeleteRepo)
}

// handleListRepos services GET /repos — the filesystem-derived repo +
// worktree listing (the repo-first replacement for the gateway-DB
// worktree list).
func (m *Mux) handleListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := m.svc.ListRepos(r.Context())
	if err != nil {
		writeRepoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, repoListResponse{Repos: repos})
}

// repoListResponse wraps the repo slice so the shape can grow without
// breaking clients. (listReposResponse is taken by the GITHUB repo
// listing in github_repos.go — different surface, different shape.)
type repoListResponse struct {
	Repos []host.RepoInfo `json:"repos"`
}

// handleRepoOverview services GET /repos/{slug}/overview — the branch ∪
// open-PR feed. ?fetch=1 refreshes origin/* with one authed fetch first.
func (m *Mux) handleRepoOverview(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "repo slug is required"})
		return
	}
	fetch := r.URL.Query().Get("fetch") == "1"
	result, err := m.svc.RepoOverview(r.Context(), slug, fetch)
	if err != nil {
		writeRepoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteRepo services DELETE /repos/{slug} — every worktree
// (sessions purged) plus the canonical. 409 worktree_busy while any
// session runs.
func (m *Mux) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "repo slug is required"})
		return
	}
	if err := m.svc.DeleteRepo(r.Context(), slug); err != nil {
		writeRepoError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	case errors.Is(err, host.ErrCannotDeleteLocalCheckout):
		writeJSON(w, http.StatusForbidden, errResp{Code: "cannot_delete_local_checkout", Error: err.Error()})
	case errors.Is(err, host.ErrBranchCheckedOutElsewhere):
		writeJSON(w, http.StatusConflict, errResp{Code: "branch_checked_out", Error: err.Error()})
	default:
		writeError(w, err)
	}
}
