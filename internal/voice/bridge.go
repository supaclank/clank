package voice

import (
	"context"
	"fmt"
	"log"

	"github.com/acksell/mindmouth/audio"
	"github.com/coder/websocket"
)

// ClientBridge manages client-side audio I/O over a WebSocket
// connection to the daemon. It owns a Recorder, Player, and the
// goroutines that shuttle PCM between them and the WebSocket.
//
// Usage:
//
//	bridge, err := voice.NewClientBridge(wsConn)
//	defer bridge.Close()
//
//	bridge.Unmute() // unmute mic — audio starts flowing to daemon
//	bridge.Mute()   // mute mic + send end signal
type ClientBridge struct {
	recorder *audio.Recorder
	player   *audio.Player
	conn     *websocket.Conn
	cancel   context.CancelFunc
}

// NewClientBridge creates audio devices, starts the mic (muted), and
// launches the send/receive goroutines that shuttle PCM between local
// audio hardware and the daemon's WebSocket.
//
// The caller must call Close when done to release all resources.
func NewClientBridge(conn *websocket.Conn) (*ClientBridge, error) {
	recorder, err := audio.NewRecorder()
	if err != nil {
		return nil, fmt.Errorf("init microphone: %w", err)
	}

	player, err := audio.NewPlayer()
	if err != nil {
		recorder.Close()
		return nil, fmt.Errorf("init speaker: %w", err)
	}

	// Start mic capture muted — caller uses Start/Stop to control.
	recorder.Mute()
	ctx, cancel := context.WithCancel(context.Background())
	micCh, err := recorder.Record(ctx)
	if err != nil {
		cancel()
		recorder.Close()
		player.Close()
		return nil, fmt.Errorf("start recording: %w", err)
	}

	// Goroutine: send mic PCM to daemon via WebSocket.
	go func() {
		for pcm := range micCh {
			if err := conn.Write(ctx, websocket.MessageBinary, pcm); err != nil {
				if ctx.Err() == nil {
					log.Printf("voice bridge: ws write error: %v", err)
				}
				return
			}
		}
	}()

	// Goroutine: receive speaker PCM from daemon, play locally.
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			// Zero-length binary message = flush signal (barge-in).
			if len(data) == 0 {
				player.Flush()
				continue
			}
			player.Enqueue(data)
		}
	}()

	return &ClientBridge{
		recorder: recorder,
		player:   player,
		conn:     conn,
		cancel:   cancel,
	}, nil
}

// Unmute unmutes the mic so audio frames start flowing to the daemon.
// The agent on the daemon side infers the start of a new sequence from
// the first audio data frame it receives.
func (b *ClientBridge) Unmute() {
	b.recorder.Unmute()
}

// Mute mutes the mic and sends an end signal to the daemon, which
// triggers the agent to commit the audio buffer and generate a response.
func (b *ClientBridge) Mute() error {
	b.recorder.Mute()
	return SendEnd(b.conn)
}

// Close cancels the audio goroutines, closes the recorder and player,
// and gracefully closes the WebSocket connection.
func (b *ClientBridge) Close() {
	b.cancel()
	b.recorder.Close()
	b.player.Close()
	b.conn.Close(websocket.StatusNormalClosure, "")
}
