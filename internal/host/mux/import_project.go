package hostmux

import "net/http"

// importProjectRequest is the body of POST /projects/import. owner/repo
// are discrete fields (not a "owner/repo" string) so the host doesn't
// re-parse, and it builds the clone URL itself — it never accepts a
// client-supplied URL.
type importProjectRequest struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
}

func (m *Mux) handleImportProject(w http.ResponseWriter, r *http.Request) {
	// 503 early when the host has no GitHub manager at all; per-request
	// "not connected" (no token) is reported by the service below.
	if _, ok := m.requireGitHub(w); !ok {
		return
	}
	var req importProjectRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: err.Error()})
		return
	}
	out, err := m.svc.ImportProjectFromGitHub(r.Context(), req.Owner, req.Repo, req.Branch)
	if err != nil {
		writeGitHubFlowErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
