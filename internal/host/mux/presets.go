package hostmux

import (
	"net/http"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/presets"
)

// GET /presets?backend= lists built-in plus user presets (built-ins
// first). The backend filter is optional; clients typically fetch all and
// group locally.
func (m *Mux) handleListPresets(w http.ResponseWriter, r *http.Request) {
	bt := agent.BackendType(r.URL.Query().Get("backend"))
	writeJSON(w, http.StatusOK, m.svc.Presets(bt))
}

// POST /presets creates or replaces a USER preset (id in the body).
// Built-in ids are reserved; writes against them fail.
func (m *Mux) handlePutPreset(w http.ResponseWriter, r *http.Request) {
	var p presets.Preset
	if err := decodeJSON(r.Body, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := m.svc.PutPreset(p); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// DELETE /presets/{id} removes a USER preset. Unknown ids 400 (fail fast)
// — built-ins are not deletable and never live in the store.
func (m *Mux) handleDeletePreset(w http.ResponseWriter, r *http.Request) {
	if err := m.svc.DeletePreset(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
