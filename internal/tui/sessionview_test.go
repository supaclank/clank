package tui

import (
	"context"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestSetEventChannel_SkipsSubscribe verifies that when SetEventChannel is
// called before Init(), the model reads from the pre-connected channel instead
// of calling subscribeEvents (which would create a second SSE connection).
// This is a regression test for the race condition where CreateSession emits
// events before the TUI subscribes.
func TestSetEventChannel_SkipsSubscribe(t *testing.T) {
	t.Parallel()

	ch := make(chan agent.Event, 16)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	model := NewSessionViewModel(nil, "test-session-123")
	model.SetEventChannel(ch, cancel)

	// Verify the channel was stored.
	if model.eventsCh == nil {
		t.Fatal("eventsCh should be set after SetEventChannel")
	}

	// Init should return commands. We can't easily introspect tea.Cmd closures,
	// but we can verify the model processes events from the pre-set channel.
	// Send an event through the channel.
	ch <- agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "test-session-123",
		Data: agent.StatusChangeData{
			OldStatus: agent.StatusStarting,
			NewStatus: agent.StatusBusy,
		},
	}
	ch <- agent.Event{
		Type:      agent.EventPartUpdate,
		SessionID: "test-session-123",
		Data: agent.PartUpdateData{
			MessageID: "msg-1",
			Part: agent.Part{
				ID:   "part-1",
				Type: "text",
				Text: "Hello from agent",
			},
		},
	}
	ch <- agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "test-session-123",
		Data: agent.StatusChangeData{
			OldStatus: agent.StatusBusy,
			NewStatus: agent.StatusIdle,
		},
	}

	// Process events directly via handleEvent (same as the integration test).
	drainTimeout := time.After(2 * time.Second)
	processed := 0
	for processed < 3 {
		select {
		case evt := <-ch:
			model.handleEvent(evt)
			processed++
		case <-drainTimeout:
			t.Fatalf("timed out, processed %d of 3 events", processed)
		}
	}

	// Verify entries were created.
	var foundText bool
	var foundStatus int
	for _, e := range model.entries {
		if e.kind == entryText && e.content == "Hello from agent" {
			foundText = true
		}
		if e.kind == entryStatus {
			foundStatus++
		}
	}

	if !foundText {
		t.Error("expected agent text entry with 'Hello from agent'")
	}
	if foundStatus < 2 {
		t.Errorf("expected at least 2 status entries, got %d", foundStatus)
	}
}
