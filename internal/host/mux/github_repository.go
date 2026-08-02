package hostmux

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

func (m *Mux) registerGitHubRepositories(mx *http.ServeMux) {
	mx.HandleFunc("POST /github/repositories/inspect", m.handleGitHubRepositoryInspect)
	mx.HandleFunc("POST /github/repositories/launch", m.handleGitHubRepositoryLaunch)
}

func (m *Mux) handleGitHubRepositoryInspect(w http.ResponseWriter, r *http.Request) {
	var locator host.GitHubRepositoryLocator
	if err := json.NewDecoder(r.Body).Decode(&locator); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "decode body: " + err.Error()})
		return
	}
	inspection, err := m.svc.InspectGitHubRepository(r.Context(), locator)
	if err != nil {
		writeGitHubRepositoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inspection)
}

func (m *Mux) handleGitHubRepositoryLaunch(w http.ResponseWriter, r *http.Request) {
	var locator host.GitHubRepositoryLocator
	if err := json.NewDecoder(r.Body).Decode(&locator); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "decode body: " + err.Error()})
		return
	}
	result, err := m.svc.LaunchGitHubRepository(r.Context(), locator)
	if err != nil {
		writeGitHubRepositoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func writeGitHubRepositoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, host.ErrGitHubManagerUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, errResp{Code: "github_unavailable", Error: err.Error()})
	case errors.Is(err, host.ErrGitHubRepositoryConnectionRequired):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_connection_required", Error: err.Error()})
	case errors.Is(err, githubpkg.ErrRepositoryTokenInvalid):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_token_invalid", Error: err.Error()})
	case errors.Is(err, githubpkg.ErrRepositoryForbidden):
		writeJSON(w, http.StatusForbidden, errResp{Code: "github_forbidden", Error: err.Error()})
	default:
		writeError(w, err)
	}
}
