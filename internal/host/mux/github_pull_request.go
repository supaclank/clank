package hostmux

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

func (m *Mux) registerGitHubPullRequests(mx *http.ServeMux) {
	mx.HandleFunc("POST /github/pull-requests/inspect", m.handleGitHubPullRequestInspect)
	mx.HandleFunc("POST /github/pull-requests/launch", m.handleGitHubPullRequestLaunch)
}

func (m *Mux) handleGitHubPullRequestInspect(w http.ResponseWriter, r *http.Request) {
	var locator host.GitHubPullRequestLocator
	if err := json.NewDecoder(r.Body).Decode(&locator); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "decode body: " + err.Error()})
		return
	}
	inspection, err := m.svc.InspectGitHubPullRequest(r.Context(), locator)
	if err != nil {
		writeGitHubPullRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inspection)
}

func (m *Mux) handleGitHubPullRequestLaunch(w http.ResponseWriter, r *http.Request) {
	var req host.GitHubPullRequestLaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "decode body: " + err.Error()})
		return
	}
	result, err := m.svc.LaunchGitHubPullRequest(r.Context(), req)
	if err != nil {
		writeGitHubPullRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func writeGitHubPullRequestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, host.ErrGitHubManagerUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, errResp{Code: "github_unavailable", Error: err.Error()})
	case errors.Is(err, host.ErrGitHubConnectionRequired):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_connection_required", Error: err.Error()})
	case errors.Is(err, host.ErrPullRequestChanged):
		writeJSON(w, http.StatusConflict, errResp{Code: "pull_request_changed", Error: err.Error()})
	case errors.Is(err, host.ErrWorktreeDirty):
		writeJSON(w, http.StatusConflict, errResp{Code: "worktree_dirty", Error: err.Error()})
	case errors.Is(err, host.ErrRemoteDiverged):
		writeJSON(w, http.StatusConflict, errResp{Code: "remote_diverged", Error: err.Error()})
	case errors.Is(err, host.ErrPullRequestLocalCommits):
		writeJSON(w, http.StatusConflict, errResp{Code: "pull_request_local_commits", Error: err.Error()})
	case errors.Is(err, host.ErrPullRequestRepoAmbiguous):
		writeJSON(w, http.StatusConflict, errResp{Code: "pull_request_repo_ambiguous", Error: err.Error()})
	case errors.Is(err, githubpkg.ErrPRTokenInvalid):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_token_invalid", Error: err.Error()})
	case errors.Is(err, githubpkg.ErrPRForbidden):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_forbidden", Error: err.Error()})
	default:
		writeError(w, err)
	}
}
