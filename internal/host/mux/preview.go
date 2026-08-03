package hostmux

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/supaclank/clank/internal/host/preview"
)

const (
	previewSelectionMaxBytes = 4 * 1024
	previewNameQuery         = "name"
	codeInvalidRequest       = "invalid_request"
	codePreviewSetupNeeded   = "preview_setup_required"
	codePreviewConfigInvalid = "preview_config_invalid"
	codePreviewNotRunning    = "not_running"
)

type previewSelection struct {
	Name string `json:"name"`
}

// handlePreviewStart spawns or returns the dev server for the URL's
// preview key — a managed worktree ID or a folder slug (laptop
// `clank preview`); the host resolves both. Idempotent — the same call
// from a slow-network mobile retry returns the existing snapshot, not
// a second spawn.
//
// The optional body selects a configured preview name. An empty body resolves
// Expo or the configured default.
func (m *Mux) handlePreviewStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: codeInvalidRequest, Error: "worktree id missing"})
		return
	}
	selection, err := decodePreviewSelection(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: codeInvalidRequest, Error: err.Error()})
		return
	}
	status, err := m.svc.PreviewStart(r.Context(), id, selection.Name)
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
		writeJSON(w, http.StatusBadRequest, errResp{Code: codeInvalidRequest, Error: "worktree id missing"})
		return
	}
	selection, err := decodePreviewSelection(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Code: codeInvalidRequest, Error: err.Error()})
		return
	}
	if err := m.svc.PreviewStop(r.Context(), id, selection.Name); err != nil {
		writePreviewError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePreviewStatus returns availability + running state for the
// URL's worktree ID. Resolution runs every call so on-disk Expo and launch
// configuration changes are reflected immediately.
func (m *Mux) handlePreviewStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: codeInvalidRequest, Error: "worktree id missing"})
		return
	}
	status, err := m.svc.PreviewStatus(r.Context(), id, r.URL.Query().Get(previewNameQuery))
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
		writeJSON(w, http.StatusBadRequest, errResp{Code: codeInvalidRequest, Error: "worktree id missing"})
		return
	}
	logs := m.svc.PreviewLogs(r.Context(), id, r.URL.Query().Get(previewNameQuery))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logs)
}

// writePreviewError translates preview package sentinels into stable wire
// codes. Other failures fall through to the host's general error mapping.
func writePreviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, preview.ErrSetupRequired):
		var setup *preview.SetupRequiredError
		if !errors.As(err, &setup) {
			writeJSON(w, http.StatusConflict, errResp{Code: codePreviewSetupNeeded, Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusConflict, errResp{
			Code:              codePreviewSetupNeeded,
			Error:             err.Error(),
			SetupPrompt:       setup.Prompt,
			ProjectConfigPath: setup.ProjectConfigPath,
		})
	case errors.Is(err, preview.ErrInvalidLaunchConfig):
		writeJSON(w, http.StatusUnprocessableEntity, errResp{Code: codePreviewConfigInvalid, Error: err.Error()})
	case errors.Is(err, preview.ErrNotRunning):
		writeJSON(w, http.StatusNotFound, errResp{Code: codePreviewNotRunning, Error: err.Error()})
	default:
		writeError(w, err)
	}
}

func decodePreviewSelection(r *http.Request) (previewSelection, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, previewSelectionMaxBytes+1))
	if err != nil {
		return previewSelection{}, err
	}
	if len(body) > previewSelectionMaxBytes {
		return previewSelection{}, errors.New("preview selection body is too large")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return previewSelection{}, nil
	}
	var selection previewSelection
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selection); err != nil {
		return previewSelection{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return previewSelection{}, errors.New("request body must contain one JSON object")
		}
		return previewSelection{}, err
	}
	return selection, nil
}
