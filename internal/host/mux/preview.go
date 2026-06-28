package hostmux

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/acksell/clank/internal/host/preview"
)

// handlePreviewStart spawns or returns the dev server for the URL's
// worktree ID. Idempotent — the same call from a slow-network mobile
// retry returns the existing snapshot, not a second spawn.
//
// No request body: the public URL is now allocated by the gateway
// (which knows the wildcard root) and surfaced in the response's
// `url` field. Callers used to supply preview_url_base; with the
// WSS-tunnel architecture the sprite mints the URL via the
// /webhooks/preview/register webhook to the gateway.
func (m *Mux) handlePreviewStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_request", Error: "worktree id missing"})
		return
	}
	// Optional body {local_path} starts an in-place preview on the
	// caller's own folder (laptop `clank preview`), keyed by {id}. An
	// empty body keeps the worktree-id path (mobile / synced worktrees).
	var body struct {
		LocalPath string `json:"local_path"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&body) // absent/!json body is the common case
	}
	var (
		status preview.Status
		err    error
	)
	if body.LocalPath != "" {
		status, err = m.svc.PreviewStartLocal(r.Context(), body.LocalPath, id)
	} else {
		status, err = m.svc.PreviewStart(r.Context(), id)
	}
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
