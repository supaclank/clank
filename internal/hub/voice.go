package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/voice"
	"github.com/coder/websocket"
)

// isBenignVoiceExit reports whether err from sess.Wait() represents an
// expected shutdown path — client closed the WebSocket, context was
// canceled by HandleVoiceAudio's defer, or the upstream voice agent
// reached end-of-stream. Surfacing these at error level was noisy and
// obscured real failures (e.g. provider auth rejections). Callers
// should still log the unusual cases.
func isBenignVoiceExit(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return true
	}
	// WebSocket normal-closure paths don't expose a sentinel; the coder
	// library wraps them into a string. Match conservatively.
	msg := err.Error()
	return strings.Contains(msg, "StatusNormalClosure") ||
		strings.Contains(msg, "StatusGoingAway") ||
		strings.Contains(msg, "use of closed network connection")
}

// HandleVoiceAudio upgrades to a WebSocket for bidirectional audio
// streaming, then creates the voice session immediately.
//
// Protocol:
//   - Client → Server text:   {"type":"end"} (end of audio sequence)
//   - Client → Server binary: PCM audio chunks (24kHz 16-bit signed LE mono)
//   - Server → Client binary: PCM audio chunks (speaker)
//   - Server → Client binary (len=0): flush signal (barge-in)
//
// There is no explicit start signal — the agent infers the start of a
// new audio sequence from the first binary data frame after an end.
//
// Only one voice session can be active at a time (singleton). The
// session is torn down when the WebSocket disconnects.
//
// This handler stays on Service (rather than moving to internal/hub/mux/)
// because it owns Service-internal singleton state (s.voice, s.voiceAudioConn)
// and a long-lived websocket whose lifecycle is tied to a goroutine on
// s.wg. The mux package delegates the route to this exported method.
func (s *Service) HandleVoiceAudio(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.voice != nil {
		s.mu.Unlock()
		writeVoiceJSON(w, http.StatusConflict, map[string]string{"error": "voice session already active"})
		return
	}
	s.mu.Unlock()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		writeVoiceJSON(w, http.StatusBadRequest, map[string]string{"error": "OPENAI_API_KEY environment variable is not set"})
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// No origin check — Unix socket is already authenticated.
	})
	if err != nil {
		s.log.Printf("voice audio websocket accept: %v", err)
		return
	}

	// Increase read limit for audio frames.
	conn.SetReadLimit(256 * 1024)

	source, sink := voice.NewWSAudioAdapters(conn)
	tp := &hubToolProvider{s: s}

	// Use the daemon's long-lived context, not the HTTP request context.
	sess, err := voice.NewSession(s.ctx, voice.Config{
		APIKey:       apiKey,
		Source:       source,
		Sink:         sink,
		ToolProvider: tp,
		Broadcast:    s.broadcast,
		Logger:       s.log,
	})
	if err != nil {
		s.log.Printf("voice session create error: %v", err)
		conn.Close(websocket.StatusInternalError, err.Error())
		return
	}

	s.mu.Lock()
	// Re-check under lock to avoid races with concurrent connects.
	if s.voice != nil {
		s.mu.Unlock()
		sess.Close()
		conn.Close(websocket.StatusPolicyViolation, "voice session already active")
		return
	}
	s.voice = sess
	s.voiceAudioConn = conn
	s.mu.Unlock()

	s.log.Println("voice audio WebSocket connected, session created")

	// Monitor voice session in background — clean up when it ends. If
	// the session exits before the client disconnects (e.g. provider
	// EOF, error), close the websocket so the handler's r.Context()
	// fires and the connection record is released. Without the close,
	// HandleVoiceAudio would block on the request context until the
	// client noticed the silence and dropped, leaking voiceAudioConn
	// in the meantime.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := sess.Wait(); err != nil && !isBenignVoiceExit(err) {
			s.log.Printf("voice session ended with error: %v", err)
		}
		s.mu.Lock()
		if s.voice == sess {
			s.voice = nil
		}
		if s.voiceAudioConn == conn {
			s.voiceAudioConn = nil
		}
		s.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "voice session ended")
	}()

	// Keep the handler alive until the connection closes — the HTTP
	// handler goroutine must outlive the WebSocket.
	<-r.Context().Done()

	s.mu.Lock()
	if s.voiceAudioConn == conn {
		s.voiceAudioConn = nil
	}
	// Tear down the voice session when the audio WebSocket disconnects.
	activeSess := s.voice
	if activeSess == sess {
		s.voice = nil
	} else {
		activeSess = nil // another session replaced ours
	}
	s.mu.Unlock()

	if activeSess != nil {
		activeSess.Close()
	}
}

// writeVoiceJSON is a tiny local helper for early-exit branches in
// HandleVoiceAudio that need to write an HTTP error before the
// websocket upgrade. Mux owns the rest of the wire format; this helper
// exists only because HandleVoiceAudio runs inside the hub package and
// can't reach hubmux helpers without an import cycle.
func writeVoiceJSON(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// hubToolProvider implements voice.ToolProvider using direct
// access to the Service's internal state and methods.
type hubToolProvider struct {
	s *Service
}

func (tp *hubToolProvider) SearchSessions(ctx context.Context, p agent.SearchParams) ([]agent.SessionInfo, error) {
	return tp.s.searchSessions(p), nil
}

func (tp *hubToolProvider) GetSession(ctx context.Context, id string) (*agent.SessionInfo, error) {
	tp.s.mu.RLock()
	ms, ok := tp.s.sessions[id]
	if !ok {
		tp.s.mu.RUnlock()
		return nil, fmt.Errorf("session not found: %s", id)
	}
	// Copy the fields we need under the lock — ms.info and ms.backend
	// are mutated under s.mu (e.g. activateBackend), so reading them
	// after RUnlock would race.
	info := ms.info
	backend := ms.backend
	tp.s.mu.RUnlock()
	if backend != nil {
		info.Status = backend.Status()
	}
	return &info, nil
}

func (tp *hubToolProvider) GetSessionMessages(ctx context.Context, sessionID string) ([]agent.MessageData, error) {
	tp.s.mu.RLock()
	ms, ok := tp.s.sessions[sessionID]
	if !ok {
		tp.s.mu.RUnlock()
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	backend := ms.backend
	tp.s.mu.RUnlock()
	if backend == nil {
		// Try to activate a read-only backend. activateBackend takes
		// s.mu internally and assigns ms.backend; re-snapshot
		// afterwards to use the assigned handle without dereferencing
		// ms outside the lock.
		if err := tp.s.activateBackend(sessionID, ms); err != nil {
			return nil, fmt.Errorf("activate backend: %w", err)
		}
		tp.s.mu.RLock()
		backend = ms.backend
		tp.s.mu.RUnlock()
		if backend == nil {
			return nil, fmt.Errorf("activate backend: no backend after activation for %s", sessionID)
		}
	}
	return backend.Messages(ctx)
}

func (tp *hubToolProvider) SendMessage(ctx context.Context, sessionID string, text string) error {
	tp.s.mu.RLock()
	ms, ok := tp.s.sessions[sessionID]
	if !ok {
		tp.s.mu.RUnlock()
		return fmt.Errorf("session not found: %s", sessionID)
	}
	backend := ms.backend
	tp.s.mu.RUnlock()
	if backend == nil {
		return fmt.Errorf("session %s has no active backend", sessionID)
	}
	// Capture ctx into a local for the goroutine. The backend call
	// must use the caller's ctx so cancelled voice/tool requests stop
	// further backend work; tp.s.ctx would only terminate at daemon
	// shutdown, defeating per-request cancellation.
	sendCtx := ctx
	go func() {
		if err := backend.SendMessage(sendCtx, agent.SendMessageOpts{Text: text}); err != nil {
			tp.s.log.Printf("voice send_message to %s: %v", sessionID, err)
			tp.s.broadcast(agent.Event{
				Type:      agent.EventError,
				SessionID: sessionID,
				Timestamp: time.Now(),
				Data:      agent.ErrorData{Message: err.Error()},
			})
		}
	}()
	return nil
}

func (tp *hubToolProvider) CreateSession(ctx context.Context, req agent.StartRequest) (*agent.SessionInfo, error) {
	return tp.s.CreateSession(ctx, req)
}

func (tp *hubToolProvider) AbortSession(ctx context.Context, sessionID string) error {
	tp.s.mu.RLock()
	ms, ok := tp.s.sessions[sessionID]
	if !ok {
		tp.s.mu.RUnlock()
		return fmt.Errorf("session not found: %s", sessionID)
	}
	backend := ms.backend
	tp.s.mu.RUnlock()
	if backend == nil {
		return fmt.Errorf("session %s has no active backend", sessionID)
	}
	return backend.Abort(ctx)
}

func (tp *hubToolProvider) KnownProjectDirs(ctx context.Context) ([]string, error) {
	if tp.s.Store == nil {
		return nil, nil
	}
	targets, err := tp.s.Store.KnownAgentTargets()
	if err != nil {
		return nil, fmt.Errorf("known agent targets: %w", err)
	}
	// Voice tools only operate on local on-disk paths. Surface the
	// distinct local-host Local-ref paths from the known targets.
	// Remote-ref repos are skipped — they may not have a stable
	// user-known checkout location (the host clones them on demand
	// to a deterministic but opaque path).
	seen := make(map[string]struct{})
	for _, t := range targets {
		if host.Hostname(t.Hostname) != host.HostLocal {
			continue
		}
		if t.GitRef.LocalPath == "" {
			continue
		}
		seen[t.GitRef.LocalPath] = struct{}{}
	}
	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}
