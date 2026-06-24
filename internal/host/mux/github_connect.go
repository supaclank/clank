package hostmux

// HTTP routes for the GitHub device-flow connect surface. The
// underlying state machine lives in internal/host/github/device_flow.go;
// this file is pure decode/dispatch/encode.
//
//   POST /credentials/github/connect/start  — kick off device flow
//   GET  /credentials/github/connect/status — poll one flow's state
//   POST /credentials/github/connect/cancel — abort an in-flight flow

import (
	"errors"
	"net/http"

	githubpkg "github.com/acksell/clank/internal/host/github"
)

// registerGitHubConnect wires the connect endpoints. Called from
// registerGitHub() in github_credentials.go alongside the status +
// disconnect routes so the whole GitHub Connect surface mounts in
// one place.
func (m *Mux) registerGitHubConnect(mx *http.ServeMux) {
	mx.HandleFunc("POST /credentials/github/connect/start", m.handleGitHubConnectStart)
	mx.HandleFunc("GET /credentials/github/connect/status", m.handleGitHubConnectStatus)
	mx.HandleFunc("POST /credentials/github/connect/cancel", m.handleGitHubConnectCancel)
}

func (m *Mux) handleGitHubConnectStart(w http.ResponseWriter, r *http.Request) {
	g, ok := m.requireGitHub(w)
	if !ok {
		return
	}
	start, err := g.StartConnect(r.Context())
	if err != nil {
		writeGitHubFlowErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, start)
}

func (m *Mux) handleGitHubConnectStatus(w http.ResponseWriter, r *http.Request) {
	g, ok := m.requireGitHub(w)
	if !ok {
		return
	}
	flowID := r.URL.Query().Get("flow_id")
	if flowID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "flow_id is required"})
		return
	}
	status, err := g.ConnectStatus(r.Context(), flowID)
	if err != nil {
		writeGitHubFlowErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (m *Mux) handleGitHubConnectCancel(w http.ResponseWriter, r *http.Request) {
	g, ok := m.requireGitHub(w)
	if !ok {
		return
	}
	flowID := r.URL.Query().Get("flow_id")
	if flowID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "flow_id is required"})
		return
	}
	if err := g.CancelConnect(r.Context(), flowID); err != nil {
		writeGitHubFlowErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeGitHubFlowErr maps domain errors from the github package to
// the right HTTP status with a stable error code clients can switch
// on. Everything else falls through to the default handler.
func writeGitHubFlowErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, githubpkg.ErrNotConfigured):
		writeJSON(w, http.StatusServiceUnavailable, errResp{
			Code:  "github_not_configured",
			Error: err.Error(),
		})
	case errors.Is(err, githubpkg.ErrNotConnected):
		writeJSON(w, http.StatusConflict, errResp{
			Code:  "github_not_connected",
			Error: err.Error(),
		})
	case errors.Is(err, githubpkg.ErrUnknownFlow):
		writeJSON(w, http.StatusNotFound, errResp{
			Code:  "unknown_flow",
			Error: err.Error(),
		})
	default:
		writeError(w, err)
	}
}
