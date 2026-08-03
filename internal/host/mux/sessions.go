package hostmux

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/oklog/ulid/v2"

	"github.com/supaclank/clank/internal/agent"
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
	// Required keys come from the backend's built-in Default preset; the
	// host never fills values in (a hidden substitution is worse than a
	// loud 400 — the client fetches the preset and sends explicitly).
	if err := m.svc.ValidateCreateConfig(req.Backend, req.Config); err != nil {
		writeError(w, err)
		return
	}
	sessionID := ulid.Make().String()
	// info is the session as persisted — GitRef normalized by the host
	// (a LocalPath inside a repo becomes {root, subdir}), so clients
	// see the same identity the store and sidebar grouping key on.
	_, info, err := m.svc.CreateSession(r.Context(), sessionID, req)
	if err != nil {
		writeError(w, err)
		return
	}

	// Open creates the remote session (sync, stamps the external ID);
	// Send is fire-and-forget on the backend's long-lived context.
	// Failure tears down the host-side registration so retry works.
	status, extID, err := m.svc.OpenAndSend(r.Context(), sessionID, agent.SendMessageOpts{
		Text:        req.Prompt,
		Model:       req.Model,
		Config:      req.Config,
		Attachments: req.Attachments,
	})
	if err != nil {
		_ = m.svc.StopSession(sessionID)
		writeError(w, fmt.Errorf("open session: %w", err))
		return
	}

	info.ExternalID = extID
	info.Status = status
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
	if msgs == nil {
		msgs = []agent.MessageData{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// PermissionReplyRequest is the body for POST
// /sessions/{id}/permissions/{permID}/reply.
type PermissionReplyRequest struct {
	Allow bool `json:"allow"`
	// Message is the reason forwarded to the model when Allow is false (e.g.
	// plan-review comments). Ignored when Allow is true.
	Message string `json:"message,omitempty"`
}

// GET /sessions/{id}/pending-permission returns the permission requests
// parked on the session's live backend, oldest first — how a client that
// (re)joins a blocked session recovers the prompt it never saw on SSE.
// In-memory only, never persisted: no live backend (e.g. after a daemon
// restart, which kills the agent and its queue together) means an
// honestly empty list, and the read never wakes a backend.
func (m *Mux) handlePendingPermissions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	perms, err := m.svc.PendingPermissions(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if perms == nil {
		perms = []agent.PermissionData{}
	}
	writeJSON(w, http.StatusOK, perms)
}

func (m *Mux) handlePermissionReply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	permID := r.PathValue("permID")
	var req PermissionReplyRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := m.svc.RespondPermission(r.Context(), id, permID, req.Allow, req.Message); err != nil {
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
	// path. Events flow once a dispatching op (Send/Abort/…) triggers
	// a rehydrate; messages GET is a pure read and doesn't either.
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
