package daemonclient

import (
	"context"
	"fmt"

	"github.com/coder/websocket"
)

// VoiceAudioStream opens a WebSocket connection for bidirectional PCM audio
// streaming. Caller sends mic PCM as binary messages and receives speaker
// PCM back. A zero-length binary message from the server signals a flush
// (barge-in / discard playback buffer). The returned *websocket.Conn must
// be closed by the caller.
func (c *Client) VoiceAudioStream(ctx context.Context) (*websocket.Conn, error) {
	conn, _, err := websocket.Dial(ctx, "ws://daemon/voice/audio", &websocket.DialOptions{
		HTTPClient: c.httpClient,
	})
	if err != nil {
		return nil, fmt.Errorf("voice audio websocket: %w", err)
	}
	conn.SetReadLimit(256 * 1024)
	return conn, nil
}
