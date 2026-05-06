package hostmux

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/acksell/clank/internal/agent"
)

// SessionSnapshot is the runtime-fields shape returned by endpoints
// where the caller already has the full SessionInfo. ServerURL is
// populated for backends that expose an HTTP server (OpenCode only).
type SessionSnapshot struct {
	SessionID  string              `json:"session_id"`
	ExternalID string              `json:"external_id"`
	Status     agent.SessionStatus `json:"status"`
	ServerURL  string              `json:"server_url,omitempty"`
}

// POST /sessions takes a StartRequest, mints a session ID, and returns
// the persisted SessionInfo.
func (m *Mux) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req agent.StartRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	sessionID := ulid.Make().String()
	_, serverURL, err := m.svc.CreateSession(r.Context(), sessionID, req)
	if err != nil {
		writeError(w, err)
		return
	}

	// Open creates the remote session (sync, stamps the external ID);
	// Send is fire-and-forget on the backend's long-lived context.
	// Failure tears down the host-side registration so retry works.
	status, extID, err := m.svc.OpenAndSend(r.Context(), sessionID, agent.SendMessageOpts{
		Text:  req.Prompt,
		Agent: req.Agent,
		Model: req.Model,
	})
	if err != nil {
		_ = m.svc.StopSession(sessionID)
		writeError(w, fmt.Errorf("open session: %w", err))
		return
	}

	now := time.Now()
	info := agent.SessionInfo{
		ID:         sessionID,
		ExternalID: extID,
		Backend:    req.Backend,
		Status:     status,
		Hostname:   req.Hostname,
		GitRef:     req.GitRef,
		Prompt:     req.Prompt,
		TicketID:   req.TicketID,
		Agent:      req.Agent,
		ServerURL:  serverURL,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	writeJSON(w, http.StatusCreated, info)
}

// GET /sessions/{id} returns the persisted SessionInfo, augmented
// with runtime fields from the live backend when registered.
// 404 if neither the store nor the live registry has it.
func (m *Mux) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, err := m.svc.GetSessionMetadata(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if b, ok := m.svc.Session(id); ok {
		info.Status = b.Status()
		if extID := b.SessionID(); extID != "" {
			info.ExternalID = extID
		}
	}
	writeJSON(w, http.StatusOK, info)
}

// handleSessionSnapshot serves the lightweight response shape used by
// the in-process host/client path; the gateway-facing GET routes to
// handleGetSession.
func (m *Mux) handleSessionSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, extID, err := m.svc.OpenSession(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, SessionSnapshot{
		SessionID:  id,
		ExternalID: extID,
		Status:     status,
	})
}

func (m *Mux) handleOpenSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, extID, err := m.svc.OpenSession(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	// Async-init backends (Claude) leave extID empty here; the caller
	// picks it up via Event.ExternalID later on the SSE stream.
	writeJSON(w, http.StatusOK, SessionSnapshot{
		SessionID:  id,
		ExternalID: extID,
		Status:     status,
	})
}

func (m *Mux) handleSendSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var opts agent.SendMessageOpts
	if err := decodeJSON(r.Body, &opts); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := m.svc.SendMessage(r.Context(), id, opts); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mux) handleOpenAndSendSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var opts agent.SendMessageOpts
	if err := decodeJSON(r.Body, &opts); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	status, extID, err := m.svc.OpenAndSend(r.Context(), id, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	// Sync-init backends (opencode) learn their sessionID inside
	// Open; without surfacing it here the client persists external_id="".
	// Async-init backends (claude) still rely on Event.ExternalID.
	writeJSON(w, http.StatusOK, SessionSnapshot{
		SessionID:  id,
		ExternalID: extID,
		Status:     status,
	})
}

func (m *Mux) handleAbortSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := m.svc.AbortSession(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevertRequest is the body for POST /sessions/{id}/revert.
type RevertRequest struct {
	MessageID string `json:"message_id"`
}

func (m *Mux) handleRevertSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req RevertRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if req.MessageID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "message_id is required"})
		return
	}
	if err := m.svc.RevertSession(r.Context(), id, req.MessageID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ForkRequest is the body for POST /sessions/{id}/fork.
type ForkRequest struct {
	MessageID string `json:"message_id"`
}

func (m *Mux) handleForkSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req ForkRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	// Empty MessageID forks the whole session ("no truncation").
	res, err := m.svc.ForkSession(r.Context(), id, req.MessageID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (m *Mux) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgs, err := m.svc.SessionMessages(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// PermissionReplyRequest is the body for POST
// /sessions/{id}/permissions/{permID}/reply.
type PermissionReplyRequest struct {
	Allow bool `json:"allow"`
}

// GET /sessions/{id}/pending-permission returns blocked permission
// requests. The host doesn't snapshot these yet — the SSE stream is
// the canonical source — so this returns an empty list to keep the
// TUI's recovery path from 404'ing.
//
// TODO: persistent permission snapshot.
func (m *Mux) handlePendingPermissions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Store-only lookup — don't rehydrate a backend just to return [].
	if _, err := m.svc.GetSessionMetadata(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, []any{})
}

func (m *Mux) handlePermissionReply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	permID := r.PathValue("permID")
	var req PermissionReplyRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := m.svc.RespondPermission(r.Context(), id, permID, req.Allow); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Mux) handleStopSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := m.svc.StopSession(id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSessionEvents streams agent events as SSE filtered to one
// session id. Subscribes to the global broadcast and filters client-
// side so multiple consumers can share one source.
//
// Encoding: `event: <type>\ndata: <json>\n\n`.
func (m *Mux) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Existence check only — don't rehydrate the backend on the SSE
	// path. Events flow once Send/Messages/etc trigger a rehydrate.
	if _, err := m.svc.GetSessionMetadata(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	subID, ch := m.svc.Subscribe()
	defer m.svc.Unsubscribe(subID)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				// Subscriber channel closed (host shutting down).
				fmt.Fprintf(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			if ev.SessionID != id {
				continue
			}
			data, err := json.Marshal(ev)
			if err != nil {
				m.log.Printf("hostmux: marshal event for %s: %v", id, err)
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		}
	}
}
