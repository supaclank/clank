package host

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/notifier"
)

// recordingProvider is a real notifier.Provider used as a test fixture
// — captures every Send and Close call. Lives in the host package so
// tests can drive the Service's private subscriberRegistry directly,
// mirroring the existing recordingListener pattern in keepalive_test.go.
type recordingNotifierProvider struct {
	mu     sync.Mutex
	sent   []notifier.Notification
	closes int
}

func (p *recordingNotifierProvider) Send(_ context.Context, n notifier.Notification) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, n)
	return nil
}

func (p *recordingNotifierProvider) Close(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return nil
}

func (p *recordingNotifierProvider) sentCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

func (p *recordingNotifierProvider) sentSnapshot() []notifier.Notification {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]notifier.Notification, len(p.sent))
	copy(out, p.sent)
	return out
}

func (p *recordingNotifierProvider) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

// newTestServiceWithNotifier constructs a Service wired with a
// recording Provider. The Service has no backends — the test publishes
// events directly via subscribers.Broadcast (the production fan-out
// point) so it can exercise the notifier wiring without spinning a
// real agent.
func newTestServiceWithNotifier(t *testing.T) (*Service, *recordingNotifierProvider) {
	t.Helper()
	rec := &recordingNotifierProvider{}
	loop := notifier.New(notifier.Config{Provider: rec})
	svc := New(Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		NotifierLoop:    loop,
	})
	return svc, rec
}

func TestNotifier_BusyToIdleEventProducesOneNotification(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithNotifier(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	svc.subscribers.Broadcast(agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	})

	if got := waitForNotifierSent(rec, 1, 500*time.Millisecond); got != 1 {
		t.Fatalf("got %d notifications, want 1", got)
	}
	got := rec.sentSnapshot()[0]
	if got.Kind != notifier.KindIdle {
		t.Errorf("Kind = %q, want %q", got.Kind, notifier.KindIdle)
	}
	if got.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", got.SessionID)
	}
}

func TestNotifier_DropsChattyEvents(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithNotifier(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	// Mix of dropped + one notification-worthy event. Use the
	// notification-worthy one as a sync point: when it arrives, the
	// loop has processed the queue in order, so any earlier dropped
	// events are guaranteed to have been examined and discarded.
	svc.subscribers.Broadcast(agent.Event{Type: agent.EventMessage, SessionID: "s1", Data: agent.MessageData{}})
	svc.subscribers.Broadcast(agent.Event{Type: agent.EventPartUpdate, SessionID: "s1", Data: agent.PartUpdateData{}})
	svc.subscribers.Broadcast(agent.Event{Type: agent.EventTitleChange, SessionID: "s1", Data: agent.TitleChangeData{Title: "hi"}})
	svc.subscribers.Broadcast(agent.Event{
		Type:      agent.EventPermission,
		SessionID: "s1",
		Data:      agent.PermissionData{RequestID: "r1", Tool: "bash"},
	})

	if got := waitForNotifierSent(rec, 1, 500*time.Millisecond); got != 1 {
		t.Fatalf("got %d notifications, want 1 (permission only)", got)
	}
	if got := rec.sentSnapshot()[0].Kind; got != notifier.KindPermission {
		t.Errorf("Kind = %q, want %q", got, notifier.KindPermission)
	}
}

func TestNotifier_ShutdownCallsProviderClose(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithNotifier(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}

	svc.Shutdown()
	if rec.closeCount() != 1 {
		t.Errorf("Provider.Close call count = %d, want 1", rec.closeCount())
	}
}

func TestNotifier_NilLoopSkipsWiring(t *testing.T) {
	t.Parallel()
	svc := New(Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		// NotifierLoop: nil
	})
	if svc.notifierLoop != nil {
		t.Error("notifierLoop should be nil when no Loop configured")
	}
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	if got := len(svc.subscribers.subs); got != 0 {
		t.Errorf("subscriber slots = %d, want 0 (no notifier subscriber)", got)
	}
}

func waitForNotifierSent(p *recordingNotifierProvider, want int, timeout time.Duration) int {
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
