package notifier

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		evt       agent.Event
		wantOK    bool
		wantKind  Kind
		wantTitle string
		wantData  map[string]any
	}{
		{
			name: "busy_to_idle_produces_kind_idle",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
			},
			wantOK:    true,
			wantKind:  KindIdle,
			wantTitle: "Agent finished",
		},
		{
			name: "starting_to_idle_produces_kind_idle",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusStarting, NewStatus: agent.StatusIdle},
			},
			wantOK:   true,
			wantKind: KindIdle,
		},
		{
			name: "idle_to_idle_dropped",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusIdle, NewStatus: agent.StatusIdle},
			},
			wantOK: false,
		},
		{
			name: "idle_to_busy_dropped",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusIdle, NewStatus: agent.StatusBusy},
			},
			wantOK: false,
		},
		{
			name: "error_to_idle_dropped",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusError, NewStatus: agent.StatusIdle},
			},
			wantOK: false,
		},
		{
			name: "busy_to_error_dropped",
			evt: agent.Event{
				Type:      agent.EventStatusChange,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusError},
			},
			wantOK: false,
		},
		{
			name: "permission_request",
			evt: agent.Event{
				Type:      agent.EventPermission,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.PermissionData{RequestID: "req-1", Tool: "bash", Description: "run `ls -la`"},
			},
			wantOK:    true,
			wantKind:  KindPermission,
			wantTitle: "Permission requested: bash",
			wantData:  map[string]any{"request_id": "req-1", "tool": "bash"},
		},
		{
			name: "permission_without_tool_name",
			evt: agent.Event{
				Type:      agent.EventPermission,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.PermissionData{RequestID: "req-2"},
			},
			wantOK:    true,
			wantKind:  KindPermission,
			wantTitle: "Permission requested",
		},
		{
			name: "error_event",
			evt: agent.Event{
				Type:      agent.EventError,
				SessionID: "s1",
				Timestamp: when,
				Data:      agent.ErrorData{Message: "model unavailable"},
			},
			wantOK:    true,
			wantKind:  KindError,
			wantTitle: "Agent error",
		},
		{
			name: "message_dropped",
			evt:  agent.Event{Type: agent.EventMessage, SessionID: "s1", Data: agent.MessageData{Role: "user"}},
		},
		{
			name: "part_dropped",
			evt:  agent.Event{Type: agent.EventPartUpdate, SessionID: "s1", Data: agent.PartUpdateData{}},
		},
		{
			name: "title_dropped",
			evt:  agent.Event{Type: agent.EventTitleChange, SessionID: "s1", Data: agent.TitleChangeData{Title: "hello"}},
		},
		{
			name: "reconnecting_dropped",
			evt:  agent.Event{Type: agent.EventReconnecting, SessionID: "s1", Data: agent.ReconnectingData{Attempt: 1}},
		},
		{
			name: "voice_status_dropped",
			evt:  agent.Event{Type: agent.EventVoiceStatus, SessionID: "s1", Data: agent.VoiceStatusData{Status: agent.VoiceStatusIdle}},
		},
		{
			name: "session_create_dropped",
			evt:  agent.Event{Type: agent.EventSessionCreate, SessionID: "s1"},
		},
		{
			name: "status_change_with_wrong_data_dropped",
			evt:  agent.Event{Type: agent.EventStatusChange, SessionID: "s1", Data: "garbage"},
		},
		{
			name: "permission_with_wrong_data_dropped",
			evt:  agent.Event{Type: agent.EventPermission, SessionID: "s1", Data: "garbage"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, ok := classify(tc.evt)
			if ok != tc.wantOK {
				t.Fatalf("classify returned ok=%v, want %v (n=%+v)", ok, tc.wantOK, n)
			}
			if !ok {
				return
			}
			if n.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", n.Kind, tc.wantKind)
			}
			if n.SessionID != tc.evt.SessionID {
				t.Errorf("SessionID = %q, want %q", n.SessionID, tc.evt.SessionID)
			}
			if tc.wantTitle != "" && n.Title != tc.wantTitle {
				t.Errorf("Title = %q, want %q", n.Title, tc.wantTitle)
			}
			if n.Body == "" {
				t.Error("Body must not be empty")
			}
			for k, want := range tc.wantData {
				got, ok := n.Data[k]
				if !ok {
					t.Errorf("Data[%q] missing", k)
					continue
				}
				if got != want {
					t.Errorf("Data[%q] = %v, want %v", k, got, want)
				}
			}
			if n.OccurredAt.IsZero() {
				t.Error("OccurredAt must be set")
			}
		})
	}
}

func TestClassify_ZeroTimestampDefaultsToNow(t *testing.T) {
	t.Parallel()
	evt := agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	}
	before := time.Now()
	n, ok := classify(evt)
	after := time.Now()
	if !ok {
		t.Fatal("expected classify to accept the event")
	}
	if n.OccurredAt.Before(before) || n.OccurredAt.After(after) {
		t.Errorf("OccurredAt = %v, want in [%v, %v]", n.OccurredAt, before, after)
	}
}

// recordingProvider is a real Provider used as a test fixture — it
// captures every Send + Close call. Not a mock; the Loop talks to it
// over the real interface.
type recordingProvider struct {
	mu       sync.Mutex
	sent     []Notification
	closes   int
	sendErr  error
	closeErr error
}

func (p *recordingProvider) Send(_ context.Context, n Notification) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, n)
	return p.sendErr
}

func (p *recordingProvider) Close(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return p.closeErr
}

func (p *recordingProvider) sentCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

func (p *recordingProvider) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

func TestLoop_DeliversNotificationWorthyEvents(t *testing.T) {
	t.Parallel()
	rec := &recordingProvider{}
	loop := New(Config{Provider: rec})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = loop.Stop(stopCtx)
	})

	loop.OnEvent(agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	})
	loop.OnEvent(agent.Event{Type: agent.EventMessage, SessionID: "s1", Data: agent.MessageData{}}) // dropped
	loop.OnEvent(agent.Event{
		Type:      agent.EventPermission,
		SessionID: "s1",
		Data:      agent.PermissionData{RequestID: "r1", Tool: "bash"},
	})

	if got := waitForSent(rec, 2, 500*time.Millisecond); got != 2 {
		t.Fatalf("got %d notifications, want 2", got)
	}
}

func TestLoop_StopCallsProviderClose(t *testing.T) {
	t.Parallel()
	rec := &recordingProvider{}
	loop := New(Config{Provider: rec})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := loop.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if rec.closeCount() != 1 {
		t.Errorf("Close call count = %d, want 1", rec.closeCount())
	}
	// Idempotent: a second Stop must not double-call Close.
	if err := loop.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if rec.closeCount() != 1 {
		t.Errorf("after second Stop, Close call count = %d, want 1 (idempotent)", rec.closeCount())
	}
}

func TestLoop_NewPanicsWithoutProvider(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Provider is nil")
		}
	}()
	_ = New(Config{})
}

func waitForSent(p *recordingProvider, want int, timeout time.Duration) int {
	deadline := time.After(timeout)
	for {
		if c := p.sentCount(); c >= want {
			return c
		}
		select {
		case <-deadline:
			return p.sentCount()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
