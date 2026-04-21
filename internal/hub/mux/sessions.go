package hubmux

import (
	"errors"
	"net/http"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/hub"
)

func (m *Mux) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req agent.StartRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeBadRequest(w, "invalid JSON: "+err.Error())
		return
	}
	info, err := m.svc.CreateSession(r.Context(), req)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (m *Mux) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, m.svc.Sessions())
}

func (m *Mux) handleSearchSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	sinceRaw := r.URL.Query().Get("since")
	untilRaw := r.URL.Query().Get("until")
	visibility := agent.SessionVisibility(r.URL.Query().Get("visibility"))

	if q == "" && sinceRaw == "" && untilRaw == "" && visibility == "" {
		writeBadRequest(w, "at least one of q, since, until, or visibility is required")
		return
	}

	p := agent.SearchParams{Query: q, Visibility: visibility}
	if sinceRaw != "" {
		t, err := agent.ParseTimeParam(sinceRaw)
		if err != nil {
			writeBadRequest(w, "invalid since param: "+err.Error())
			return
		}
		p.Since = t
	}
	if untilRaw != "" {
		t, err := agent.ParseTimeParam(untilRaw)
		if err != nil {
			writeBadRequest(w, "invalid until param: "+err.Error())
			return
		}
		p.Until = t
	}
	writeJSON(w, http.StatusOK, m.svc.SearchSessions(p))
}

func (m *Mux) handleGetSession(w http.ResponseWriter, r *http.Request) {
	info, err := m.svc.GetSession(r.PathValue("id"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (m *Mux) handleGetSessionMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := m.svc.SessionMessages(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (m *Mux) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text  string               `json:"text"`
		Agent string               `json:"agent"`
		Model *agent.ModelOverride `json:"model,omitempty"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON")
		return
	}
	if body.Text == "" {
		writeBadRequest(w, "text is required")
		return
	}
	in := hub.SendMessageInput{Text: body.Text, Agent: body.Agent, Model: body.Model}
	if err := m.svc.SendMessage(r.Context(), r.PathValue("id"), in); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

func (m *Mux) handleAbortSession(w http.ResponseWriter, r *http.Request) {
	if err := m.svc.AbortSession(r.Context(), r.PathValue("id")); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

func (m *Mux) handleRevertSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MessageID string `json:"message_id"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON")
		return
	}
	if body.MessageID == "" {
		writeBadRequest(w, "message_id is required")
		return
	}
	if err := m.svc.RevertSession(r.Context(), r.PathValue("id"), body.MessageID); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reverted"})
}

func (m *Mux) handleForkSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MessageID string `json:"message_id"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON")
		return
	}
	info, err := m.svc.ForkSession(r.Context(), r.PathValue("id"), body.MessageID)
	// Partial-success: the fork created a session row (info != nil) but
	// activating its backend failed. Surface the cause via a header so
	// existing clients keep working (body remains a SessionInfo) while
	// curious clients can read X-Partial-Activation-Error to diagnose
	// why the new session has no live backend yet.
	if err != nil && errors.Is(err, hub.ErrPartialActivation) && info != nil {
		w.Header().Set("X-Partial-Activation-Error", err.Error())
		writeJSON(w, http.StatusOK, info)
		return
	}
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (m *Mux) handleMarkSessionRead(w http.ResponseWriter, r *http.Request) {
	if err := m.svc.MarkSessionRead(r.PathValue("id")); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (m *Mux) handleToggleFollowUp(w http.ResponseWriter, r *http.Request) {
	followUp, err := m.svc.ToggleSessionFollowUp(r.PathValue("id"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"follow_up": followUp})
}

func (m *Mux) handleSetVisibility(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Visibility agent.SessionVisibility `json:"visibility"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON")
		return
	}
	if err := m.svc.SetSessionVisibility(r.PathValue("id"), body.Visibility); err != nil {
		writeServiceErr(w, err)
		return
	}
	// Legacy returns 200 with no body. Preserve.
	w.WriteHeader(http.StatusOK)
}

func (m *Mux) handleSetDraft(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Draft string `json:"draft"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON")
		return
	}
	if err := m.svc.SetSessionDraft(r.PathValue("id"), body.Draft); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (m *Mux) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := m.svc.DeleteSession(r.Context(), r.PathValue("id")); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (m *Mux) handleDiscoverSessions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProjectDir string `json:"project_dir"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		writeBadRequest(w, "invalid JSON")
		return
	}
	if body.ProjectDir == "" {
		writeBadRequest(w, "project_dir is required")
		return
	}
	res, err := m.svc.DiscoverSessions(r.Context(), body.ProjectDir)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"discovered": res.Discovered,
		"total":      res.Total,
	})
}

// hub import is referenced via hub.SendMessageInput above; keep it.
var _ = hub.SendMessageInput{}
