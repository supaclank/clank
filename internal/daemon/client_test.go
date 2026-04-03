package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestParseSSEStream_LargePayload verifies that SSE events with payloads
// exceeding the old bufio.Scanner 1MB limit are parsed correctly.
// Regression test for "bufio.Scanner: token too long".
func TestParseSSEStream_LargePayload(t *testing.T) {
	t.Parallel()

	// 2MB text — well above the old 1MB scanner limit.
	largeText := strings.Repeat("a", 2*1024*1024)

	evt := agent.Event{
		Type:      agent.EventPartUpdate,
		Timestamp: time.Now().Truncate(time.Millisecond),
		Data: agent.PartUpdateData{
			MessageID: "msg-1",
			Part: agent.Part{
				ID:   "part-large",
				Type: agent.PartText,
				Text: largeText,
			},
		},
	}
	jsonData, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Build an SSE stream: event + data + blank line terminator.
	ssePayload := fmt.Sprintf("event: part_update\ndata: %s\n\n", jsonData)

	ch := make(chan agent.Event, 8)
	go parseSSEStream(strings.NewReader(ssePayload), ch)

	select {
	case got := <-ch:
		data, ok := got.Data.(agent.PartUpdateData)
		if !ok {
			t.Fatalf("Data type = %T, want agent.PartUpdateData", got.Data)
		}
		if len(data.Part.Text) != 2*1024*1024 {
			t.Errorf("Part.Text length = %d, want %d", len(data.Part.Text), 2*1024*1024)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// TestParseSSEStream_StreamError verifies that non-EOF read errors are
// reported as EventError on the channel instead of being silently dropped.
func TestParseSSEStream_StreamError(t *testing.T) {
	t.Parallel()

	ch := make(chan agent.Event, 8)
	go parseSSEStream(&errReader{err: fmt.Errorf("connection reset")}, ch)

	select {
	case got := <-ch:
		if got.Type != agent.EventError {
			t.Fatalf("event type = %s, want %s", got.Type, agent.EventError)
		}
		data, ok := got.Data.(agent.ErrorData)
		if !ok {
			t.Fatalf("Data type = %T, want agent.ErrorData", got.Data)
		}
		if !strings.Contains(data.Message, "connection reset") {
			t.Errorf("error message = %q, want it to contain %q", data.Message, "connection reset")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for error event")
	}
}

// errReader is an io.Reader that always returns the configured error.
type errReader struct {
	err error
}

func (r *errReader) Read([]byte) (int, error) {
	return 0, r.err
}
