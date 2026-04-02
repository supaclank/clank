package voice

import (
	"context"
	"log"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// wsSource implements mindmouth.AudioSource, receiving PCM audio from
// a WebSocket connection (the CLI streams mic data to the daemon).
type wsSource struct {
	conn *websocket.Conn

	mu     sync.Mutex
	muted  atomic.Int32
	ch     chan []byte
	cancel context.CancelFunc
}

// NewWSAudioAdapters creates a paired AudioSource and AudioSink that
// communicate over a single WebSocket connection. The source reads
// binary messages from the client (mic PCM), and the sink writes
// binary messages back (speaker PCM).
func NewWSAudioAdapters(conn *websocket.Conn) (*wsSource, *wsSink) {
	return newWSSource(conn), newWSSink(conn)
}

func newWSSource(conn *websocket.Conn) *wsSource {
	return &wsSource{conn: conn}
}

// Record starts reading binary WebSocket messages and forwarding them
// as PCM audio chunks. The channel is closed when ctx is cancelled or
// the connection drops.
func (s *wsSource) Record(ctx context.Context) (<-chan []byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	ch := make(chan []byte, 64)
	s.ch = ch

	// Start muted (caller unmutes via StartListening).
	s.muted.Store(1)

	go func() {
		defer close(ch)
		defer cancel()
		var received, forwarded, dropped int64
		for {
			_, data, err := s.conn.Read(ctx)
			if err != nil {
				log.Printf("wsSource: exiting (forwarded=%d dropped=%d): %v", forwarded, dropped, err)
				return
			}
			received++
			if s.muted.Load() == 1 {
				continue // drop audio while muted
			}
			select {
			case ch <- data:
				forwarded++
			default:
				dropped++
				if dropped == 1 || dropped%50 == 0 {
					log.Printf("wsSource: dropped chunk (consumer slow), total=%d", dropped)
				}
			}
		}
	}()

	return ch, nil
}

func (s *wsSource) Mute()   { s.muted.Store(1) }
func (s *wsSource) Unmute() { s.muted.Store(0) }

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
