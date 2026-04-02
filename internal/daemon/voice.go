package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/voice"
	"github.com/coder/websocket"
)

// handleVoiceStart creates a new voice session. Only one voice session
// can be active at a time (singleton). The caller must first connect
// an audio WebSocket via /voice/audio before starting the voice session.
func (d *Daemon) handleVoiceStart(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	if d.voice != nil {
		d.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "voice session already active"})
		return
	}
	if d.voiceAudioConn == nil {
		d.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connect audio WebSocket first (GET /voice/audio)"})
		return
	}
	d.mu.Unlock()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "OPENAI_API_KEY environment variable is not set"})
		return
	}

	tp := &daemonToolProvider{d: d}

	d.mu.Lock()
	source, sink := d.voiceAudioSource, d.voiceAudioSink
	d.mu.Unlock()

	// Use the daemon's long-lived context, not the HTTP request context
	// which is cancelled as soon as the response is written.
	sess, err := voice.NewSession(d.ctx, voice.Config{
		APIKey:       apiKey,
		Source:       source,
		Sink:         sink,
		ToolProvider: tp,
		Broadcast:    d.broadcast,
		Logger:       d.log,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	d.mu.Lock()
	d.voice = sess
	d.mu.Unlock()

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

	writeJSON(w, http.StatusCreated, map[string]string{"status": "voice session started"})
}

// handleVoiceStop tears down the active voice session.
func (d *Daemon) handleVoiceStop(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	sess := d.voice
	d.voice = nil
	conn := d.voiceAudioConn
	d.voiceAudioConn = nil
	d.voiceAudioSource = nil
	d.voiceAudioSink = nil
	d.mu.Unlock()

	if sess != nil {
		sess.Close()
	}
	if conn != nil {
		conn.CloseNow()
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "voice session stopped"})
}

// handleVoiceListen starts a user voice turn (unmute mic).
func (d *Daemon) handleVoiceListen(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	sess := d.voice
	d.mu.RUnlock()

	if sess == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no active voice session"})
		return
	}

	sess.StartListening()
	writeJSON(w, http.StatusOK, map[string]string{"status": "listening"})
}

// handleVoiceUnlisten ends a user voice turn (mute mic, trigger response).
func (d *Daemon) handleVoiceUnlisten(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	sess := d.voice
	d.mu.RUnlock()

	if sess == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no active voice session"})
		return
	}

	sess.StopListening()
	writeJSON(w, http.StatusOK, map[string]string{"status": "processing"})
}

// handleVoiceAudio upgrades to a WebSocket for bidirectional PCM
// audio streaming. The client sends mic PCM as binary messages; the
// server sends speaker PCM back as binary messages. A zero-length
// binary message from the server signals a flush (barge-in).
//
// This must be connected before calling POST /voice/start.
func (d *Daemon) handleVoiceAudio(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// No origin check — Unix socket is already authenticated.
	})
	if err != nil {
		d.log.Printf("voice audio websocket accept: %v", err)
		return
	}

	// Increase read limit for audio frames.
	conn.SetReadLimit(256 * 1024)

	d.mu.Lock()
	if d.voiceAudioConn != nil {
		d.mu.Unlock()
		conn.Close(websocket.StatusPolicyViolation, "audio connection already established")
		return
	}
	source, sink := voice.NewWSAudioAdapters(conn)
	d.voiceAudioConn = conn
	d.voiceAudioSource = source
	d.voiceAudioSink = sink
	d.mu.Unlock()

	d.log.Println("voice audio WebSocket connected")

	// Keep the handler alive until the connection closes — the HTTP
	// handler goroutine must outlive the WebSocket.
	<-r.Context().Done()

	d.mu.Lock()
	if d.voiceAudioConn == conn {
		d.voiceAudioConn = nil
		d.voiceAudioSource = nil
		d.voiceAudioSink = nil
	}
	// Also tear down the voice session if audio disconnects.
	sess := d.voice
	d.voice = nil
	d.mu.Unlock()

	if sess != nil {
		sess.Close()
	}
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

func (tp *daemonToolProvider) ListSessions(ctx context.Context) ([]agent.SessionInfo, error) {
	return tp.d.snapshotSessions(), nil
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
