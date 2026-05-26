package hostmux

import (
	"errors"
	"net/http"

	"github.com/acksell/clank/internal/host/preview"
)

// previewStartRequest is the body of POST /worktrees/{id}/preview/start.
//
// PreviewURLBase is the full public URL Metro will bake into its
// manifest output (launchAsset.url and friends). The client owns this:
// mobile knows its gateway URL, laptop curl tests know localhost. The
// sprite cannot guess because it doesn't know the user's gateway/user
// prefix.
type previewStartRequest struct {
	PreviewURLBase string `json:"preview_url_base"`
}

// handlePreviewStart spawns or returns the dev server for the URL's
// worktree ID. Idempotent — the same call from a slow-network mobile
// retry returns the existing snapshot, not a second spawn.
func (m *Mux) handlePreviewStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_request", Error: "worktree id missing"})
		return
	}
	var req previewStartRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_request", Error: err.Error()})
		return
	}
	if req.PreviewURLBase == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_request", Error: "preview_url_base is required"})
		return
	}

	status, err := m.svc.PreviewStart(r.Context(), id, req.PreviewURLBase)
	if err != nil {
		writePreviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handlePreviewStop terminates the dev server for the URL's worktree
// ID. 404 with code=not_running when nothing's running — lets mobile's
// "close preview" handler be naively idempotent.
func (m *Mux) handlePreviewStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_request", Error: "worktree id missing"})
		return
	}
	if err := m.svc.PreviewStop(r.Context(), id); err != nil {
		writePreviewError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePreviewStatus returns availability + running state for the
// URL's worktree ID. Detect runs every call so the Available bit
// reflects on-disk truth (the user might have just removed package.json
// inside an agent session).
func (m *Mux) handlePreviewStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_request", Error: "worktree id missing"})
		return
	}
	status, err := m.svc.PreviewStatus(r.Context(), id)
	if err != nil {
		writePreviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handlePreviewLogs returns the captured stdout/stderr tail for the
// worktree's dev server. Returned as text/plain because the typical
// consumer (developer's curl, mobile loading screen) just wants the
// bytes — not JSON-quoted line array. Returns 200 + empty body when
// the server isn't running rather than 404; lets clients poll without
// special-casing "not started yet".
func (m *Mux) handlePreviewLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_request", Error: "worktree id missing"})
		return
	}
	logs := m.svc.PreviewLogs(id)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logs)
}

// writePreviewError translates preview package sentinels into the wire
// shape clients expect. ErrNotPreviewable → no_preview, ErrNotRunning
// → not_running. Anything else (workdir resolution failures, etc.)
// falls through to the existing host writeError.
func writePreviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, preview.ErrNotPreviewable):
		writeJSON(w, http.StatusNotFound, errResp{Code: "no_preview", Error: err.Error()})
	case errors.Is(err, preview.ErrNotRunning):
		writeJSON(w, http.StatusNotFound, errResp{Code: "not_running", Error: err.Error()})
	default:
		writeError(w, err)
	}
}
