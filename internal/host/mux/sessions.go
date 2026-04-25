package hostmux

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// CreateSessionRequest is the body for POST /sessions.
//
// SessionID is the Hub-assigned ULID under which the Host registers the
// new SessionBackend. The Host does not generate session IDs; the Hub
// owns that responsibility because the Hub holds the durable registry.
type CreateSessionRequest struct {
	SessionID string             `json:"session_id"`
	Request   agent.StartRequest `json:"request"`
}

// SessionSnapshot summarizes a registered session for HTTP responses. It
// is not the same as agent.SessionInfo (Hub-side metadata); it carries
// only what the Host knows about its own backend instance.
//
// ServerURL is populated on POST /sessions responses for backends that
// expose an HTTP server (currently only OpenCode); empty otherwise. The
// Hub uses it for per-session shell-out (e.g. `opencode attach <url>`).
// Other endpoints (GET /sessions/{id}) leave it empty.
type SessionSnapshot struct {
	SessionID  string              `json:"session_id"`
	ExternalID string              `json:"external_id"`
	Status     agent.SessionStatus `json:"status"`
	ServerURL  string              `json:"server_url,omitempty"`
}

// HOST
func (m *Mux) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "session_id is required"})
		return
	}
	b, serverURL, err := m.svc.CreateSession(r.Context(), req.SessionID, req.Request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, SessionSnapshot{
		SessionID:  req.SessionID,
		ExternalID: b.SessionID(),
		Status:     b.Status(),
		ServerURL:  serverURL,
	})
}

// HOST
func (m *Mux) handleSessionSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, SessionSnapshot{
		SessionID:  id,
		ExternalID: b.SessionID(),
		Status:     b.Status(),
	})
}

// HOST
func (m *Mux) handleOpenSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
		return
	}
	if err := b.Open(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	// Return the post-Open snapshot so the client can refresh its
	// cached ExternalID/Status. The ID may not be known yet for
	// async-init backends (e.g. Claude); the caller will pick it up
	// later via Event.ExternalID on the SSE stream.
	writeJSON(w, http.StatusOK, SessionSnapshot{
		SessionID:  id,
		ExternalID: b.SessionID(),
		Status:     b.Status(),
	})
}

// HOST
func (m *Mux) handleSendSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
		return
	}
	var opts agent.SendMessageOpts
	if err := decodeJSON(r.Body, &opts); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := b.Send(r.Context(), opts); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HOST
func (m *Mux) handleOpenAndSendSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
		return
	}
	var opts agent.SendMessageOpts
	if err := decodeJSON(r.Body, &opts); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := b.OpenAndSend(r.Context(), opts); err != nil {
		writeError(w, err)
		return
	}
	// Return the post-OpenAndSend snapshot so the client can refresh
	// its cached ExternalID/Status. Some backends (opencode) learn
	// their sessionID inside Open synchronously; without this the
	// client keeps the empty ExternalID it received from POST
	// /sessions and persists external_id="" on the Hub. Async-init
	// backends (claude) still rely on Event.ExternalID.
	writeJSON(w, http.StatusOK, SessionSnapshot{
		SessionID:  id,
		ExternalID: b.SessionID(),
		Status:     b.Status(),
	})
}

// HOST
func (m *Mux) handleAbortSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
		return
	}
	if err := b.Abort(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevertRequest is the body for POST /sessions/{id}/revert.
type RevertRequest struct {
	MessageID string `json:"message_id"`
}

// HOST
func (m *Mux) handleRevertSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
		return
	}
	var req RevertRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if req.MessageID == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "message_id is required"})
		return
	}
	if err := b.Revert(r.Context(), req.MessageID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ForkRequest is the body for POST /sessions/{id}/fork.
type ForkRequest struct {
	MessageID string `json:"message_id"`
}

// HOST
func (m *Mux) handleForkSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
		return
	}
	var req ForkRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	// Empty MessageID is valid: it forks the entire session from
	// scratch. Backends interpret it as "no truncation point".
	res, err := b.Fork(r.Context(), req.MessageID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// HOST
func (m *Mux) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
		return
	}
	msgs, err := b.Messages(r.Context())
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

// HOST
func (m *Mux) handlePermissionReply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	permID := r.PathValue("permID")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
		return
	}
	var req PermissionReplyRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err := b.RespondPermission(r.Context(), permID, req.Allow); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HOST
func (m *Mux) handleStopSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := m.svc.StopSession(id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HOST
//
// handleSessionEvents bridges SessionBackend.Events() to Server-Sent
// Events. The handler holds the response open until either the client
// disconnects (ctx done) or the backend's Events channel closes.
//
// Each event is encoded as `event: <type>\ndata: <json>\n\n`. The HTTP
// adapter on the Hub side reconstructs an agent.Event channel from this
// stream.
func (m *Mux) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, ok := m.svc.Session(id)
	if !ok {
		writeError(w, fmt.Errorf("session %s: %w", id, host.ErrNotFound))
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

	ch := b.Events()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				// Backend stopped — signal end of stream and return.
				fmt.Fprintf(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
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
