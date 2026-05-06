package daemoncli

// Event-stream round-trip coverage. Ported from internal/hub/events_test.go
// (deleted with the hub in PR 3 phase 3c). Each test drives the full
// pipeline:
//
//   stub backend → host.Service.relayBackendEvents → subscriberRegistry
//   → /events SSE → daemonclient.Sessions().Subscribe
//
// The subscriberRegistry is new in PR 3 (events used to fan out from
// the hub). These tests pin that the wire still carries every event
// type with concrete data — a class of regression that would otherwise
// only surface in manual TUI testing.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestEventRoundTrip_StatusChange verifies the host stamps a status
// change event on session create and the daemonclient receives it
// with the concrete StatusChangeData type intact (not a generic map
// from the JSON decoder).
func TestEventRoundTrip_StatusChange(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	_, b := td.CreateOpenCodeSession(t, "hello")
	// Drive a Starting → Busy transition by pushing the event ourselves.
	go b.PushEvent(agent.Event{
		Type:      agent.EventStatusChange,
		Timestamp: time.Now(),
		Data: agent.StatusChangeData{
			OldStatus: agent.StatusStarting,
			NewStatus: agent.StatusBusy,
		},
	})

	statusEvt, all := receiveEventsByType(t, events, agent.EventStatusChange, 2*time.Second)
	if statusEvt == nil {
		t.Fatalf("no status change event received; drained: %d events", len(all))
	}
	data, ok := statusEvt.Data.(agent.StatusChangeData)
	if !ok {
		t.Fatalf("Event.Data type = %T, want agent.StatusChangeData", statusEvt.Data)
	}
	if data.OldStatus != agent.StatusStarting || data.NewStatus != agent.StatusBusy {
		t.Errorf("status change: %v → %v, want %v → %v",
			data.OldStatus, data.NewStatus, agent.StatusStarting, agent.StatusBusy)
	}
}

// TestEventRoundTrip_InjectedTypes pins that every event type the TUI
// renders survives the backend → host SSE → client JSON round-trip
// with its concrete type intact. Without this, a quiet regression
// (e.g. dropping a registered Data type) would silently downgrade
// events to map[string]interface{} and break the renderer at runtime.
func TestEventRoundTrip_InjectedTypes(t *testing.T) {
	cases := []struct {
		name  string
		event agent.Event
		check func(t *testing.T, evt agent.Event)
	}{
		{
			name: "PartUpdate-text",
			event: agent.Event{
				Type:      agent.EventPartUpdate,
				Timestamp: time.Now(),
				Data: agent.PartUpdateData{
					MessageID: "msg-123",
					Part: agent.Part{ID: "p1", Type: agent.PartText, Text: "hi"},
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				d, ok := evt.Data.(agent.PartUpdateData)
				if !ok {
					t.Fatalf("Data type = %T, want PartUpdateData", evt.Data)
				}
				if d.MessageID != "msg-123" || d.Part.Text != "hi" {
					t.Errorf("PartUpdate = %+v", d)
				}
			},
		},
		{
			name: "PartUpdate-tool-call",
			event: agent.Event{
				Type:      agent.EventPartUpdate,
				Timestamp: time.Now(),
				Data: agent.PartUpdateData{
					Part: agent.Part{ID: "t1", Type: agent.PartToolCall, Tool: "bash", Text: "ls -la", Status: agent.PartRunning},
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				d, ok := evt.Data.(agent.PartUpdateData)
				if !ok {
					t.Fatalf("Data type = %T", evt.Data)
				}
				if d.Part.Tool != "bash" || d.Part.Status != agent.PartRunning {
					t.Errorf("ToolPart = %+v", d.Part)
				}
			},
		},
		{
			name: "Message",
			event: agent.Event{
				Type:      agent.EventMessage,
				Timestamp: time.Now(),
				Data: agent.MessageData{
					Role:    "assistant",
					Content: "Here is my response",
					Parts:   []agent.Part{{ID: "p1", Type: agent.PartText, Text: "Here is my response"}},
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				d, ok := evt.Data.(agent.MessageData)
				if !ok {
					t.Fatalf("Data type = %T", evt.Data)
				}
				if d.Role != "assistant" || d.Content != "Here is my response" {
					t.Errorf("Message = %+v", d)
				}
			},
		},
		{
			name: "Permission",
			event: agent.Event{
				Type:      agent.EventPermission,
				Timestamp: time.Now(),
				Data: agent.PermissionData{
					RequestID:   "perm-789",
					Tool:        "bash",
					Description: "rm -rf /",
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				d, ok := evt.Data.(agent.PermissionData)
				if !ok {
					t.Fatalf("Data type = %T", evt.Data)
				}
				if d.RequestID != "perm-789" || d.Tool != "bash" {
					t.Errorf("Permission = %+v", d)
				}
			},
		},
		{
			name: "Error",
			event: agent.Event{
				Type:      agent.EventError,
				Timestamp: time.Now(),
				Data:      agent.ErrorData{Message: "boom"},
			},
			check: func(t *testing.T, evt agent.Event) {
				d, ok := evt.Data.(agent.ErrorData)
				if !ok {
					t.Fatalf("Data type = %T", evt.Data)
				}
				if d.Message != "boom" {
					t.Errorf("Error.Message = %q", d.Message)
				}
			},
		},
		{
			name: "TitleChange",
			event: agent.Event{
				Type:      agent.EventTitleChange,
				Timestamp: time.Now(),
				Data:      agent.TitleChangeData{Title: "Fix login bug"},
			},
			check: func(t *testing.T, evt agent.Event) {
				d, ok := evt.Data.(agent.TitleChangeData)
				if !ok {
					t.Fatalf("Data type = %T", evt.Data)
				}
				if d.Title != "Fix login bug" {
					t.Errorf("Title = %q", d.Title)
				}
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			td := newTestDaemon(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			events, err := td.Client.Sessions().Subscribe(ctx)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			_, b := td.CreateOpenCodeSession(t, "hello")
			go b.PushEvent(c.event)

			got, _ := receiveEventsByType(t, events, c.event.Type, 2*time.Second)
			if got == nil {
				t.Fatalf("no %s event received", c.event.Type)
			}
			c.check(t, *got)
		})
	}
}

// TestEventRoundTrip_StreamingTextDeltas verifies that multiple
// PartUpdate events for the same Part.ID arrive in order with the
// correct text — the SSE renderer relies on this for live streaming.
func TestEventRoundTrip_StreamingTextDeltas(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	_, b := td.CreateOpenCodeSession(t, "hello")

	deltas := []string{"Hello", " world", "!"}
	go func() {
		for _, d := range deltas {
			b.PushEvent(agent.Event{
				Type:      agent.EventPartUpdate,
				Timestamp: time.Now(),
				Data: agent.PartUpdateData{
					Part: agent.Part{ID: "text-1", Type: agent.PartText, Text: d},
				},
			})
		}
	}()

	// Drain part-update events; ignore other events that happen to
	// flow concurrently (status changes from CreateSession, etc).
	got := make([]string, 0, len(deltas))
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
loop:
	for len(got) < len(deltas) {
		select {
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			if ev.Type != agent.EventPartUpdate {
				continue
			}
			d, ok := ev.Data.(agent.PartUpdateData)
			if !ok || d.Part.ID != "text-1" {
				continue
			}
			got = append(got, d.Part.Text)
		case <-deadline.C:
			break loop
		}
	}

	if strings.Join(got, "") != strings.Join(deltas, "") {
		t.Errorf("delta order/content: got %q, want %q", got, deltas)
	}
}

// TestEventRoundTrip_SessionIDStamped verifies that events broadcast
// by the host carry the session ID even if the backend didn't set
// one. This is what lets the TUI route events to the right session
// view.
func TestEventRoundTrip_SessionIDStamped(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	info, b := td.CreateOpenCodeSession(t, "hello")
	go b.PushEvent(agent.Event{
		Type:      agent.EventPartUpdate,
		Timestamp: time.Now(),
		Data:      agent.PartUpdateData{Part: agent.Part{ID: "x", Type: agent.PartText, Text: "hi"}},
	})

	got, _ := receiveEventsByType(t, events, agent.EventPartUpdate, 2*time.Second)
	if got == nil {
		t.Fatal("no PartUpdate received")
	}
	if got.SessionID != info.ID {
		t.Errorf("event.SessionID = %q, want %q (host should stamp it)", got.SessionID, info.ID)
	}
}

// TestEventRoundTrip_MultipleSubscribers verifies that two concurrent
// Subscribe() calls each receive the same events. The host's
// subscriberRegistry uses a non-blocking fan-out with bounded buffers;
// regression coverage for slow-consumer drop semantics lives in
// host/events_test.go — this just pins the happy path through the wire.
func TestEventRoundTrip_MultipleSubscribers(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe a: %v", err)
	}
	bch, err := td.Client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe b: %v", err)
	}

	_, backend := td.CreateOpenCodeSession(t, "hi")
	go backend.PushEvent(agent.Event{
		Type:      agent.EventStatusChange,
		Timestamp: time.Now(),
		Data:      agent.StatusChangeData{OldStatus: agent.StatusStarting, NewStatus: agent.StatusBusy},
	})

	gotA, _ := receiveEventsByType(t, a, agent.EventStatusChange, 2*time.Second)
	gotB, _ := receiveEventsByType(t, bch, agent.EventStatusChange, 2*time.Second)
	if gotA == nil || gotB == nil {
		t.Fatalf("subscribers a=%v b=%v should both receive the event", gotA, gotB)
	}
}
