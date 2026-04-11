package voice

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/acksell/mindmouth"
	"github.com/coder/websocket"
)

// wsEvent is the JSON structure for in-band control messages sent as
// WebSocket text messages. Binary messages carry PCM audio data.
type wsEvent struct {
	Type string `json:"type"` // "end"
}

// SendEnd sends an end signal over the WebSocket as a text message,
// indicating that the current audio sequence is complete.
func SendEnd(conn *websocket.Conn) error {
	data, err := json.Marshal(wsEvent{Type: "end"})
	if err != nil {
		return err
	}
	return conn.Write(context.Background(), websocket.MessageText, data)
}

// wsSource implements mindmouth.AudioSource by reading from a WebSocket
// connection. The client sends:
//   - MessageText with {"type":"end"} for end-of-sequence
//   - MessageBinary for PCM audio chunks
//
// There is no explicit start signal — the agent infers the start of a
// new sequence from the first audio data frame after an end.
// Boundary signals and audio travel on the same WebSocket, eliminating
// the race between HTTP POSTs and audio data arrival.
type wsSource struct {
	conn *websocket.Conn
}

// NewWSAudioAdapters creates a paired AudioSource and AudioSink that
// communicate over a single WebSocket connection. The source reads
// from the client (control signals + PCM), and the sink writes
// binary messages back (speaker PCM).
func NewWSAudioAdapters(conn *websocket.Conn) (*wsSource, *wsSink) {
	return &wsSource{conn: conn}, newWSSink(conn)
}

// Receive implements mindmouth.AudioSource. It reads WebSocket messages
// and translates them into AudioFrames:
//   - Binary message (len > 0)      → AudioFrameData
//   - Text message {"type":"end"}   → AudioFrameEnd
//
// The channel is closed when ctx is cancelled or the connection drops.
func (s *wsSource) Receive(ctx context.Context) (<-chan mindmouth.AudioFrame, error) {
	ch := make(chan mindmouth.AudioFrame, 64)

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
				var ev wsEvent
				if err := json.Unmarshal(data, &ev); err != nil {
					log.Printf("wsSource: invalid text message: %v", err)
					continue
				}
				switch ev.Type {
				case "end":
					select {
					case ch <- mindmouth.AudioFrame{Type: mindmouth.AudioFrameEnd}:
					case <-ctx.Done():
						return
					}
				default:
					log.Printf("wsSource: unknown event type: %q", ev.Type)
				}

			case websocket.MessageBinary:
				if len(data) == 0 {
					continue // ignore zero-length binary (reserved for sink flush)
				}
				select {
				case ch <- mindmouth.AudioFrame{Type: mindmouth.AudioFrameData, PCM: data}:
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
