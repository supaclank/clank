package hostmux

// HTTP routes for the GitHub Connect surface. PR 1 wires the status
// + delete endpoints; PR 2 adds device-flow connect/start/status/cancel
// in github_connect.go; PR 3 adds POST /worktrees/{id}/pr in
// github_pr.go. See internal/host/github for the underlying logic;
// this file is pure decode/dispatch/encode.

import (
	"net/http"

	githubpkg "github.com/acksell/clank/internal/host/github"
)

// registerGitHub wires the /credentials/github/* and
// /worktrees/{id}/pr routes onto mx. Called from register() in
// mux.go.
func (m *Mux) registerGitHub(mx *http.ServeMux) {
	mx.HandleFunc("GET /credentials/github/status", m.handleGitHubStatus)
	mx.HandleFunc("DELETE /credentials/github", m.handleGitHubDisconnect)
	m.registerGitHubConnect(mx)
	m.registerGitHubPR(mx)
}

// requireGitHub fetches the Service-bound GitHub manager and returns
// 503 when it's missing (homedir resolution failed at startup). When
// the manager exists but ClientID isn't configured, callers handle
// that case explicitly — status returns available:false and connect
// would 503 with a distinct code.
func (m *Mux) requireGitHub(w http.ResponseWriter) (*githubpkg.Manager, bool) {
	g := m.svc.GitHub()
	if g == nil {
		writeJSON(w, http.StatusServiceUnavailable, errResp{
			Code:  "github_unavailable",
			Error: "github manager is not configured on this host",
		})
		return nil, false
	}
	return g, true
}

func (m *Mux) handleGitHubStatus(w http.ResponseWriter, r *http.Request) {
	g, ok := m.requireGitHub(w)
	if !ok {
		return
	}
	status, err := g.Status(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (m *Mux) handleGitHubDisconnect(w http.ResponseWriter, r *http.Request) {
	g, ok := m.requireGitHub(w)
	if !ok {
		return
	}
	if err := g.Disconnect(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
