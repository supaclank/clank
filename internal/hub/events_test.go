package hub_test

import (
	"context"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/hub"
)

func TestDaemonPing(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	resp, err := client.PingInfo(ctx)
	if err != nil {
		t.Fatalf("PingInfo: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status=ok, got %s", resp.Status)
	}
	if resp.PID == 0 {
		t.Error("expected non-zero PID")
	}
	if resp.Version == "" {
		t.Error("expected non-empty version")
	}
}

func TestDaemonStatus(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()

	// Create a session.
	_, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Sessions() is populated synchronously inside Create, but the
	// status handler reads a snapshot; poll instead of a fixed sleep
	// so a slow CI doesn't flake and a fast run doesn't idle.
	waitFor(t, 2*time.Second, "session appears in status", func() bool {
		st, err := client.Status(ctx)
		return err == nil && len(st.Sessions) == 1
	})

	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if status.PID == 0 {
		t.Error("expected non-zero PID")
	}
	if len(status.Sessions) != 1 {
		t.Errorf("expected 1 session in status, got %d", len(status.Sessions))
	}
}

func TestDaemonGracefulShutdownStopsBackends(t *testing.T) {
	mgr := newMockBackendManager()

	s := hub.New()
	s.BackendManagers[agent.BackendOpenCode] = mgr
	s.BackendManagers[agent.BackendClaudeCode] = mgr

	client, _, cleanup := startHubOnSocket(t, s)
	// Note: cleanup is NOT deferred — this test triggers shutdown
	// inline to verify backends are stopped during graceful shutdown.
	registerTestRepo(t, s)

	ctx := context.Background()

	// Create two sessions.
	_, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "task a",
	})
	if err != nil {
		t.Fatalf("CreateSession a: %v", err)
	}
	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendClaudeCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "task b",
	})
	if err != nil {
		t.Fatalf("CreateSession b: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Trigger shutdown via cleanup (s.Stop + wait for Run to return).
	cleanup()

	// All backends should have been stopped.
	backends := mgr.getAll()
	for i, b := range backends {
		b.mu.Lock()
		if !b.stopped {
			t.Errorf("backend %d was not stopped during shutdown", i)
		}
		b.mu.Unlock()
	}
}

func TestDaemonEventStream(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Subscribe to events.
	events, err := client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	// Create a session — should generate events.
	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// We should receive a session.create event and a status change event.
	received := make([]agent.Event, 0)
	timeout := time.After(2 * time.Second)

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Fatal("event channel closed unexpectedly")
			}
			received = append(received, evt)
			if len(received) >= 2 {
				goto done
			}
		case <-timeout:
			goto done
		}
	}
done:

	if len(received) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(received))
	}

	// First event should be session.create.
	if received[0].Type != agent.EventSessionCreate {
		t.Errorf("expected first event type=session.create, got %s", received[0].Type)
	}

	// Second should be status change (starting -> busy).
	if received[1].Type != agent.EventStatusChange {
		t.Errorf("expected second event type=status, got %s", received[1].Type)
	}
}

func TestDaemonMultipleEventSubscribers(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Two subscribers.
	events1, err := client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents 1: %v", err)
	}
	events2, err := client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents 2: %v", err)
	}

	// Create a session.
	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Both should receive events.
	waitForEvent := func(ch <-chan agent.Event, name string) {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Errorf("%s: channel closed", name)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: timed out waiting for event", name)
		}
	}

	waitForEvent(events1, "subscriber1")
	waitForEvent(events2, "subscriber2")
}

func TestEventRoundTrip_StatusChange(t *testing.T) {
	_, client, _, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	received := receiveEvents(events, 3, 2*time.Second)

	var statusEvt *agent.Event
	for i := range received {
		if received[i].Type == agent.EventStatusChange {
			statusEvt = &received[i]
			break
		}
	}
	if statusEvt == nil {
		t.Fatalf("no status change event received; got %d events: %v", len(received), eventTypes(received))
	}

	data, ok := statusEvt.Data.(agent.StatusChangeData)
	if !ok {
		t.Fatalf("Event.Data type = %T, want agent.StatusChangeData", statusEvt.Data)
	}
	if data.OldStatus != agent.StatusStarting {
		t.Errorf("OldStatus = %q, want %q", data.OldStatus, agent.StatusStarting)
	}
	if data.NewStatus != agent.StatusBusy {
		t.Errorf("NewStatus = %q, want %q", data.NewStatus, agent.StatusBusy)
	}
}

// TestEventRoundTrip_InjectedEvents verifies that various event types survive
// the backend -> daemon SSE -> client JSON round-trip with correct concrete types.
func TestEventRoundTrip_InjectedEvents(t *testing.T) {
	tests := []struct {
		name  string
		event agent.Event
		check func(t *testing.T, evt agent.Event)
	}{
		{
			name: "PartUpdate",
			event: agent.Event{
				Type:      agent.EventPartUpdate,
				Timestamp: time.Now(),
				Data: agent.PartUpdateData{
					MessageID: "msg-123",
					Part: agent.Part{
						ID:   "part-456",
						Type: agent.PartText,
						Text: "Hello world",
					},
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.PartUpdateData)
				if !ok {
					t.Fatalf("Data type = %T, want PartUpdateData", evt.Data)
				}
				if data.MessageID != "msg-123" {
					t.Errorf("MessageID = %q, want %q", data.MessageID, "msg-123")
				}
				if data.Part.ID != "part-456" || data.Part.Type != agent.PartText || data.Part.Text != "Hello world" {
					t.Errorf("Part = %+v, unexpected", data.Part)
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
					Parts: []agent.Part{
						{ID: "p1", Type: agent.PartText, Text: "Here is my response"},
					},
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.MessageData)
				if !ok {
					t.Fatalf("Data type = %T, want MessageData", evt.Data)
				}
				if data.Role != "assistant" || data.Content != "Here is my response" {
					t.Errorf("Role=%q Content=%q, unexpected", data.Role, data.Content)
				}
				if len(data.Parts) != 1 || data.Parts[0].ID != "p1" {
					t.Errorf("Parts = %+v, unexpected", data.Parts)
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
					Description: "Run: rm -rf /",
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.PermissionData)
				if !ok {
					t.Fatalf("Data type = %T, want PermissionData", evt.Data)
				}
				if data.RequestID != "perm-789" || data.Tool != "bash" || data.Description != "Run: rm -rf /" {
					t.Errorf("Data = %+v, unexpected", data)
				}
			},
		},
		{
			name: "Error",
			event: agent.Event{
				Type:      agent.EventError,
				Timestamp: time.Now(),
				Data: agent.ErrorData{
					Message: "something went wrong",
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.ErrorData)
				if !ok {
					t.Fatalf("Data type = %T, want ErrorData", evt.Data)
				}
				if data.Message != "something went wrong" {
					t.Errorf("Message = %q, want %q", data.Message, "something went wrong")
				}
			},
		},
		{
			name: "ToolPartWithStatus",
			event: agent.Event{
				Type:      agent.EventPartUpdate,
				Timestamp: time.Now(),
				Data: agent.PartUpdateData{
					Part: agent.Part{
						ID:     "tool-1",
						Type:   agent.PartToolCall,
						Tool:   "bash",
						Text:   "ls -la",
						Status: agent.PartRunning,
					},
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.PartUpdateData)
				if !ok {
					t.Fatalf("Data type = %T, want PartUpdateData", evt.Data)
				}
				if data.Part.Tool != "bash" || data.Part.Status != agent.PartRunning || data.Part.Text != "ls -la" {
					t.Errorf("Part = %+v, unexpected", data.Part)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
			defer cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			events, err := client.Sessions().Subscribe(ctx)
			if err != nil {
				t.Fatalf("SubscribeEvents: %v", err)
			}

			_, err = client.Sessions().Create(ctx, agent.StartRequest{
				Backend: agent.BackendOpenCode,
				GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
				Prompt:  "hello",
			})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			time.Sleep(100 * time.Millisecond)
			receiveEvents(events, 3, 500*time.Millisecond) // drain initial events

			b := getBackend()
			if b == nil {
				t.Fatal("no backend created")
			}
			b.events <- tt.event

			received := receiveEvents(events, 1, 2*time.Second)
			if len(received) == 0 {
				t.Fatal("no event received")
			}
			if received[0].Type != tt.event.Type {
				t.Fatalf("event type = %q, want %q", received[0].Type, tt.event.Type)
			}
			tt.check(t, received[0])
		})
	}
}

// TestEventRoundTrip_StreamingTextDeltas verifies that multiple PartUpdate
// events with text deltas all arrive on the client with correct Part.ID and
// Part.Text, simulating streaming output from an agent.
func TestEventRoundTrip_StreamingTextDeltas(t *testing.T) {
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	receiveEvents(events, 3, 500*time.Millisecond)

	b := getBackend()
	if b == nil {
		t.Fatal("no backend created")
	}

	deltas := []string{"Hello", " world", "!"}
	for _, delta := range deltas {
		b.events <- agent.Event{
			Type:      agent.EventPartUpdate,
			Timestamp: time.Now(),
			Data: agent.PartUpdateData{
				Part: agent.Part{
					ID:   "text-part-1",
					Type: agent.PartText,
					Text: delta,
				},
			},
		}
	}

	received := receiveEvents(events, 3, 2*time.Second)
	if len(received) != 3 {
		t.Fatalf("expected 3 events, got %d", len(received))
	}

	for i, evt := range received {
		data, ok := evt.Data.(agent.PartUpdateData)
		if !ok {
			t.Errorf("event[%d].Data type = %T, want agent.PartUpdateData", i, evt.Data)
			continue
		}
		if data.Part.ID != "text-part-1" {
			t.Errorf("event[%d].Part.ID = %q, want %q", i, data.Part.ID, "text-part-1")
		}
		if data.Part.Text != deltas[i] {
			t.Errorf("event[%d].Part.Text = %q, want %q", i, data.Part.Text, deltas[i])
		}
	}
}

// TestEventRoundTrip_SessionID verifies that the daemon stamps SessionID
// on events before broadcasting, and that it survives the round-trip.
func TestEventRoundTrip_SessionID(t *testing.T) {
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	receiveEvents(events, 3, 500*time.Millisecond)

	b := getBackend()
	if b == nil {
		t.Fatal("no backend created")
	}

	b.events <- agent.Event{
		Type:      agent.EventPartUpdate,
		Timestamp: time.Now(),
		Data: agent.PartUpdateData{
			Part: agent.Part{ID: "x", Type: agent.PartText, Text: "hi"},
		},
	}

	received := receiveEvents(events, 1, 2*time.Second)
	if len(received) == 0 {
		t.Fatal("no event received")
	}
	if received[0].SessionID != info.ID {
		t.Errorf("SessionID = %q, want %q (daemon should stamp it)", received[0].SessionID, info.ID)
	}
}

// TestEventRoundTrip_TitleChange verifies that TitleChangeData survives the
// backend -> daemon SSE -> client JSON round-trip as a concrete type.
func TestEventRoundTrip_TitleChange(t *testing.T) {
	t.Parallel()
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.Sessions().Subscribe(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	receiveEvents(events, 3, 500*time.Millisecond) // drain initial events

	b := getBackend()
	if b == nil {
		t.Fatal("no backend created")
	}
	b.events <- agent.Event{
		Type:      agent.EventTitleChange,
		Timestamp: time.Now(),
		Data: agent.TitleChangeData{
			Title: "Fix authentication bug in login flow",
		},
	}

	received := receiveEvents(events, 1, 2*time.Second)
	if len(received) == 0 {
		t.Fatal("no event received")
	}
	if received[0].Type != agent.EventTitleChange {
		t.Fatalf("event type = %q, want %q", received[0].Type, agent.EventTitleChange)
	}
	data, ok := received[0].Data.(agent.TitleChangeData)
	if !ok {
		t.Fatalf("Data type = %T, want agent.TitleChangeData", received[0].Data)
	}
	if data.Title != "Fix authentication bug in login flow" {
		t.Errorf("Title = %q, want %q", data.Title, "Fix authentication bug in login flow")
	}
}

// TestDaemonTitleUpdateOnSession verifies that when a backend emits a title
// change event, the daemon updates the session info's Title field.
