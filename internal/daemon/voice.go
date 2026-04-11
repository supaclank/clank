package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/voice"
	"github.com/coder/websocket"
)

// handleVoiceAudio upgrades to a WebSocket for bidirectional audio and
// turn-signal streaming, then creates the voice session immediately.
//
// Protocol:
//   - Client → Server text:   {"type":"start"} / {"type":"end"}
//   - Client → Server binary: PCM audio chunks (24kHz 16-bit signed LE mono)
//   - Server → Client binary: PCM audio chunks (speaker)
//   - Server → Client binary (len=0): flush signal (barge-in)
//
// Only one voice session can be active at a time (singleton). The
// session is torn down when the WebSocket disconnects.
func (d *Daemon) handleVoiceAudio(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	if d.voice != nil {
		d.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "voice session already active"})
		return
	}
	d.mu.Unlock()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "OPENAI_API_KEY environment variable is not set"})
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// No origin check — Unix socket is already authenticated.
	})
	if err != nil {
		d.log.Printf("voice audio websocket accept: %v", err)
		return
	}

	// Increase read limit for audio frames.
	conn.SetReadLimit(256 * 1024)

	source, sink := voice.NewWSAudioAdapters(conn)
	tp := &daemonToolProvider{d: d}

	// Use the daemon's long-lived context, not the HTTP request context.
	sess, err := voice.NewSession(d.ctx, voice.Config{
		APIKey:       apiKey,
		Source:       source,
		Sink:         sink,
		ToolProvider: tp,
		Broadcast:    d.broadcast,
		Logger:       d.log,
	})
	if err != nil {
		d.log.Printf("voice session create error: %v", err)
		conn.Close(websocket.StatusInternalError, err.Error())
		return
	}

	d.mu.Lock()
	// Re-check under lock to avoid races with concurrent connects.
	if d.voice != nil {
		d.mu.Unlock()
		sess.Close()
		conn.Close(websocket.StatusPolicyViolation, "voice session already active")
		return
	}
	d.voice = sess
	d.voiceAudioConn = conn
	d.mu.Unlock()

	d.log.Println("voice audio WebSocket connected, session created")

	// Monitor voice session in background — clean up when it ends.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		if err := sess.Wait(); err != nil {
			d.log.Printf("voice session ended: %v", err)
		}
		d.mu.Lock()
		if d.voice == sess {
			d.voice = nil
		}
		d.mu.Unlock()
	}()

	// Keep the handler alive until the connection closes — the HTTP
	// handler goroutine must outlive the WebSocket.
	<-r.Context().Done()

	d.mu.Lock()
	if d.voiceAudioConn == conn {
		d.voiceAudioConn = nil
	}
	// Tear down the voice session when the audio WebSocket disconnects.
	activeSess := d.voice
	if activeSess == sess {
		d.voice = nil
	} else {
		activeSess = nil // another session replaced ours
	}
	d.mu.Unlock()

	if activeSess != nil {
		activeSess.Close()
	}
}

// handleVoiceStop tears down the active voice session.
func (d *Daemon) handleVoiceStop(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	sess := d.voice
	d.voice = nil
	conn := d.voiceAudioConn
	d.voiceAudioConn = nil
	d.mu.Unlock()

	if sess != nil {
		sess.Close()
	}
	if conn != nil {
		conn.CloseNow()
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "voice session stopped"})
}

// handleVoiceStatus returns the current voice session state.
func (d *Daemon) handleVoiceStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	sess := d.voice
	d.mu.RUnlock()

	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]string{"active": "false", "status": string(agent.VoiceStatusIdle)})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"active": "true",
		"status": string(sess.Status()),
	})
}

// daemonToolProvider implements voice.ToolProvider using direct
// access to the Daemon's internal state and methods.
type daemonToolProvider struct {
	d *Daemon
}

func (tp *daemonToolProvider) SearchSessions(ctx context.Context, p agent.SearchParams) ([]agent.SessionInfo, error) {
	return tp.d.searchSessions(p), nil
}

func (tp *daemonToolProvider) GetSession(ctx context.Context, id string) (*agent.SessionInfo, error) {
	tp.d.mu.RLock()
	ms, ok := tp.d.sessions[id]
	tp.d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	info := ms.info
	if ms.backend != nil {
		info.Status = ms.backend.Status()
	}
	return &info, nil
}

func (tp *daemonToolProvider) GetSessionMessages(ctx context.Context, sessionID string) ([]agent.MessageData, error) {
	tp.d.mu.RLock()
	ms, ok := tp.d.sessions[sessionID]
	tp.d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	if ms.backend == nil {
		// Try to activate a read-only backend.
		if err := tp.d.activateBackend(sessionID, ms); err != nil {
			return nil, fmt.Errorf("activate backend: %w", err)
		}
	}
	return ms.backend.Messages(ctx)
}

func (tp *daemonToolProvider) SendMessage(ctx context.Context, sessionID string, text string) error {
	tp.d.mu.RLock()
	ms, ok := tp.d.sessions[sessionID]
	tp.d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	if ms.backend == nil {
		return fmt.Errorf("session %s has no active backend", sessionID)
	}
	go func() {
		if err := ms.backend.SendMessage(tp.d.ctx, agent.SendMessageOpts{Text: text}); err != nil {
			tp.d.log.Printf("voice send_message to %s: %v", sessionID, err)
			tp.d.broadcast(agent.Event{
				Type:      agent.EventError,
				SessionID: sessionID,
				Timestamp: time.Now(),
				Data:      agent.ErrorData{Message: err.Error()},
			})
		}
	}()
	return nil
}

func (tp *daemonToolProvider) CreateSession(ctx context.Context, req agent.StartRequest) (*agent.SessionInfo, error) {
	return tp.d.createSession(req)
}

func (tp *daemonToolProvider) AbortSession(ctx context.Context, sessionID string) error {
	tp.d.mu.RLock()
	ms, ok := tp.d.sessions[sessionID]
	tp.d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	if ms.backend == nil {
		return fmt.Errorf("session %s has no active backend", sessionID)
	}
	return ms.backend.Abort(ctx)
}

func (tp *daemonToolProvider) KnownProjectDirs(ctx context.Context) ([]string, error) {
	if tp.d.Store == nil {
		return nil, nil
	}
	seen := make(map[string]struct{})
	for bt := range tp.d.BackendManagers {
		dirs, err := tp.d.Store.KnownProjectDirs(bt)
		if err != nil {
			return nil, fmt.Errorf("known dirs for %s: %w", bt, err)
		}
		for _, dir := range dirs {
			seen[dir] = struct{}{}
		}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}
