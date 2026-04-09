package voice

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/acksell/mindmouth"
	"github.com/coder/websocket"
)

// turnSignal is the JSON structure for in-band turn signals sent as
// WebSocket text messages. Binary messages carry PCM audio data.
type turnSignal struct {
	Type string `json:"type"` // "turn_start" or "turn_end"
}

// wsSource implements mindmouth.AudioSource by reading from a WebSocket
// connection. The client sends:
//   - MessageText with {"type":"turn_start"} / {"type":"turn_end"} for turn boundaries
//   - MessageBinary for PCM audio chunks
//
// This replaces the old Record/Mute/Unmute API: turn management is now
// in-band on the same WebSocket, eliminating the race between HTTP
// listen/unlisten POSTs and audio data arrival.
type wsSource struct {
	conn *websocket.Conn
}

// NewWSAudioAdapters creates a paired AudioSource and AudioSink that
// communicate over a single WebSocket connection. The source reads
// from the client (turn signals + mic PCM), and the sink writes
// binary messages back (speaker PCM).
func NewWSAudioAdapters(conn *websocket.Conn) (*wsSource, *wsSink) {
	return &wsSource{conn: conn}, newWSSink(conn)
}

// Stream implements mindmouth.AudioSource. It reads WebSocket messages
// and translates them into AudioEvents:
//   - Text message {"type":"turn_start"} → EventTurnStart
//   - Binary message (len > 0)           → EventAudio
//   - Text message {"type":"turn_end"}   → EventTurnEnd
//
// The channel is closed when ctx is cancelled or the connection drops.
func (s *wsSource) Stream(ctx context.Context) (<-chan mindmouth.AudioEvent, error) {
	ch := make(chan mindmouth.AudioEvent, 64)

	go func() {
		defer close(ch)
		for {
			typ, data, err := s.conn.Read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("wsSource: read error: %v", err)
				}
				return
			}

			switch typ {
			case websocket.MessageText:
				var sig turnSignal
				if err := json.Unmarshal(data, &sig); err != nil {
					log.Printf("wsSource: invalid text message: %v", err)
					continue
				}
				switch sig.Type {
				case "turn_start":
					select {
					case ch <- mindmouth.AudioEvent{Type: mindmouth.EventTurnStart}:
					case <-ctx.Done():
						return
					}
				case "turn_end":
					select {
					case ch <- mindmouth.AudioEvent{Type: mindmouth.EventTurnEnd}:
					case <-ctx.Done():
						return
					}
				default:
					log.Printf("wsSource: unknown signal type: %q", sig.Type)
				}

			case websocket.MessageBinary:
				if len(data) == 0 {
					continue // ignore zero-length binary (reserved for sink flush)
				}
				select {
				case ch <- mindmouth.AudioEvent{Type: mindmouth.EventAudio, PCM: data}:
				default:
					// Drop audio if consumer is slow — same policy as before.
					log.Printf("wsSource: dropped audio chunk (consumer slow)")
				}
			}
		}
	}()

	return ch, nil
}

// wsSink implements mindmouth.AudioSink, sending PCM audio back to
// the CLI over a WebSocket connection for local playback.
type wsSink struct {
	conn *websocket.Conn

	mu      sync.Mutex
	flushed bool
	chunks  int64 // total audio chunks sent
}

func newWSSink(conn *websocket.Conn) *wsSink {
	return &wsSink{conn: conn}
}

// Enqueue sends a PCM audio chunk to the client for playback.
func (s *wsSink) Enqueue(pcm []byte) {
	s.mu.Lock()
	if s.flushed {
		s.flushed = false
	}
	s.chunks++
	n := s.chunks
	s.mu.Unlock()

	if n == 1 {
		log.Printf("wsSink: first audio chunk (%d bytes)", len(pcm))
	}

	// Best-effort write — if the connection is slow, we drop.
	if err := s.conn.Write(context.Background(), websocket.MessageBinary, pcm); err != nil {
		log.Printf("wsSink: write error on chunk #%d: %v", n, err)
	}
}

// Flush signals the client to discard buffered audio (barge-in).
// We send a special zero-length binary message as the flush signal.
func (s *wsSink) Flush() {
	s.mu.Lock()
	s.flushed = true
	sent := s.chunks
	s.chunks = 0
	s.mu.Unlock()

	log.Printf("wsSink: flush (sent %d audio chunks this turn)", sent)

	// Zero-length binary message = flush signal.
	_ = s.conn.Write(context.Background(), websocket.MessageBinary, []byte{})
}
