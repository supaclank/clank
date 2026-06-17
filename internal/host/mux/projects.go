package hostmux

import (
	"net/http"
)

// createProjectRequest is the body of POST /projects/create. The host is
// a generic executor here: it clones the concrete clone_url the gateway
// resolved from its template catalog. The host never resolves template
// ids or accepts a client-supplied URL — the gateway is the gatekeeper.
type createProjectRequest struct {
	CloneURL string `json:"clone_url"`
	Name     string `json:"name"`
}

// HOST
func (m *Mux) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if req.CloneURL == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "clone_url is required"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "name is required"})
		return
	}
	out, err := m.svc.CreateProjectFromTemplate(r.Context(), req.CloneURL, req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
