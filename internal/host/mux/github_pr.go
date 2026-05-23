package hostmux

// HTTP route for POST /worktrees/{id}/pr. See internal/host/github_pr.go
// for the orchestration; this file is pure decode/dispatch/encode +
// error classification.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

func (m *Mux) registerGitHubPR(mx *http.ServeMux) {
	mx.HandleFunc("POST /worktrees/{id}/pr", m.handleGitHubCreatePR)
}

func (m *Mux) handleGitHubCreatePR(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "worktree id is required"})
		return
	}
	var req host.CreatePRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "decode body: " + err.Error()})
		return
	}
	result, err := m.svc.CreatePR(r.Context(), id, req)
	if err != nil {
		writePRError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// writePRError maps the typed errors from Service.CreatePR (and the
// github + git packages it composes) to HTTP statuses + stable
// machine codes. Anything unknown falls through to writeError.
//
// The 409 cases are particularly important: branch_already_has_pr
// includes the existing PR URL in the body so the UI can deep-link
// instead of just showing "conflict".
func writePRError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, host.ErrGitHubManagerUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, errResp{Code: "github_unavailable", Error: err.Error()})
	case errors.Is(err, host.ErrGitHubNotConnected):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_not_connected", Error: err.Error()})
	case errors.Is(err, host.ErrPRMissingField):
		writeJSON(w, http.StatusBadRequest, errResp{Code: "missing_field", Error: err.Error()})
	case errors.Is(err, host.ErrNothingToPush):
		writeJSON(w, http.StatusBadRequest, errResp{Code: "nothing_to_push", Error: err.Error()})
	case errors.Is(err, host.ErrNoOriginRemote):
		writeJSON(w, http.StatusBadRequest, errResp{Code: "no_origin_remote", Error: err.Error()})
	case errors.Is(err, githubpkg.ErrNotGitHubRemote):
		writeJSON(w, http.StatusBadRequest, errResp{Code: "not_github_remote", Error: err.Error()})
	case errors.Is(err, git.ErrPushNotFastForward):
		writeJSON(w, http.StatusConflict, errResp{Code: "not_fast_forward", Error: err.Error()})
	case errors.Is(err, git.ErrPushRepoNotFound):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_repo_not_accessible", Error: err.Error()})
	case errors.Is(err, git.ErrPushPermissionDenied):
		writeJSON(w, http.StatusForbidden, errResp{Code: "push_denied", Error: err.Error()})
	case errors.Is(err, githubpkg.ErrPRAlreadyExists):
		writeJSON(w, http.StatusConflict, prAlreadyExistsResp{
			Code:        "branch_already_has_pr",
			Error:       err.Error(),
			ExistingURL: githubpkg.ExistingURLFromError(err),
		})
	case errors.Is(err, githubpkg.ErrPRBaseNotFound):
		writeJSON(w, http.StatusBadRequest, errResp{Code: "base_branch_not_found", Error: err.Error()})
	case errors.Is(err, githubpkg.ErrPRTokenInvalid):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_token_invalid", Error: err.Error()})
	case errors.Is(err, githubpkg.ErrPRForbidden):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_forbidden", Error: err.Error()})
	default:
		writeError(w, err)
	}
}

// prAlreadyExistsResp carries the existing PR URL alongside the
// usual code/error pair so clients can render a "View existing PR"
// button instead of a dead-end conflict toast.
type prAlreadyExistsResp struct {
	Code        string `json:"code"`
	Error       string `json:"error"`
	ExistingURL string `json:"existing_url,omitempty"`
}
