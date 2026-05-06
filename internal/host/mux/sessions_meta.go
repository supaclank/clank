package hostmux

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/store"
)

// handleListSessions returns every persisted session, newest-updated
// first. The TUI's inbox uses this as its primary feed.
func (m *Mux) handleListSessions(w http.ResponseWriter, r *http.Request) {
	infos, err := m.svc.ListSessionMetadata(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if infos == nil {
		infos = []agent.SessionInfo{}
	}
	writeJSON(w, http.StatusOK, infos)
}

// handleSearchSessions filters persisted sessions by query, visibility,
// and time range. Used by the TUI's inbox search.
//
// Query parameters:
//
//	q          (string)         substring match on title/prompt/draft/project_dir
//	visibility (string)         exact match: "" | "done" | "archived"
//	since      (RFC3339)        updated_at >= since
//	until      (RFC3339)        updated_at <= until
//	limit      (int)            cap results; default 500
func (m *Mux) handleSearchSessions(w http.ResponseWriter, r *http.Request) {
	params := store.SearchParams{
		Q:          r.URL.Query().Get("q"),
		Visibility: agent.SessionVisibility(r.URL.Query().Get("visibility")),
	}
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			params.Limit = n
		}
	}
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_since", Error: err.Error()})
			return
		}
		params.Since = t
	}
	if s := r.URL.Query().Get("until"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_until", Error: err.Error()})
			return
		}
		params.Until = t
	}
	infos, err := m.svc.SearchSessionMetadata(r.Context(), params)
	if err != nil {
		writeError(w, err)
		return
	}
	if infos == nil {
		infos = []agent.SessionInfo{}
	}
	writeJSON(w, http.StatusOK, infos)
}

func (m *Mux) handleMarkSessionRead(w http.ResponseWriter, r *http.Request) {
	if err := m.svc.MarkSessionRead(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mux) handleToggleSessionFollowUp(w http.ResponseWriter, r *http.Request) {
	info, err := m.svc.ToggleSessionFollowUp(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

type setVisibilityRequest struct {
	Visibility agent.SessionVisibility `json:"visibility"`
}

func (m *Mux) handleSetSessionVisibility(w http.ResponseWriter, r *http.Request) {
	var body setVisibilityRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := m.svc.SetSessionVisibility(r.Context(), r.PathValue("id"), body.Visibility); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type setDraftRequest struct {
	Draft string `json:"draft"`
}

func (m *Mux) handleSetSessionDraft(w http.ResponseWriter, r *http.Request) {
	var body setDraftRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := m.svc.SetSessionDraft(r.Context(), r.PathValue("id"), body.Draft); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mux) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Stop live backend first so the agent process exits before we
	// drop metadata. Idempotent on missing session.
	if err := m.svc.StopSession(id); err != nil && !errors.Is(err, host.ErrNotFound) {
		writeError(w, fmt.Errorf("stop session %s: %w", id, err))
		return
	}
	if err := m.svc.DeleteSessionMetadata(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
