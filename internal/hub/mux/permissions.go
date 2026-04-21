package hubmux

import "net/http"

func (m *Mux) handlePermissionReply(w http.ResponseWriter, r *http.Request) {
	// allow is a *bool so we can distinguish a missing/null field from
	// an explicit false. Defaulting to false silently denies permissions
	// that the client may have intended to allow but encoded incorrectly.
	var body struct {
		Allow *bool `json:"allow"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON")
		return
	}
	if body.Allow == nil {
		writeBadRequest(w, "missing required field: allow")
		return
	}
	if err := m.svc.RespondPermission(r.Context(), r.PathValue("id"), r.PathValue("permID"), *body.Allow); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (m *Mux) handleGetPendingPermission(w http.ResponseWriter, r *http.Request) {
	perms, err := m.svc.PendingPermissions(r.PathValue("id"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, perms)
}
