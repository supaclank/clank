package host

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/keepalive"
)

// recordingListener is a real keepalive.Listener used as a test fixture
// — captures every Tick + Close call. Lives in the host package so
// tests can drive the Service's private subscriberRegistry directly
// (the host's relayBackendEvents is the production path; the
// regression test bypasses backends and publishes events directly so
// it can exercise the keepalive wiring without spinning a real agent).
type recordingListener struct {
	mu     sync.Mutex
	ticks  []time.Time
	closed bool
}

func (l *recordingListener) Tick(_ context.Context, lastActivity time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ticks = append(l.ticks, lastActivity)
}

func (l *recordingListener) Close(_ context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

func (l *recordingListener) lastNonZeroTick() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.ticks) - 1; i >= 0; i-- {
		if !l.ticks[i].IsZero() {
			return l.ticks[i]
		}
	}
	return time.Time{}
}

func (l *recordingListener) tickCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ticks)
}

func (l *recordingListener) wasClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// newTestServiceWithKeepalive constructs a Service wired with a
// recording Listener. The Service has no backends — the test publishes
// events directly via subscribers.Broadcast (the production flow's
// fanout point) so it can exercise the keepalive wiring in isolation.
func newTestServiceWithKeepalive(t *testing.T) (*Service, *recordingListener) {
	t.Helper()
	rec := &recordingListener{}
	svc := New(Options{
		BackendManagers:   map[agent.BackendType]agent.BackendManager{},
		KeepaliveListener: rec,
	})
	// The Service was constructed with a real keepalive.Loop using
	// the default 30s/5s tunables. Swap in a sub-second one so the
	// test runs fast.
	svc.keepaliveLoop = keepalive.New(keepalive.Config{
		Listener:        rec,
		Interval:        30 * time.Millisecond,
		MinTickInterval: 1 * time.Millisecond,
	})
	return svc, rec
}

// TestRegression_MobileExitDoesNotKillBusyAgent is the bug-reproducing
// test. Before the keepalive wiring, mobile-client exit closed the
// gateway's outbound SSE to the sprite, which let the sprite's
// last-consumer timer hibernate the VM and kill running agents. With
// the wiring, backend events continue to flow through the
// subscriberRegistry → keepalive.Loop → Listener, so the provider's
// lease stays renewed even when no external SSE client is attached.
//
// We simulate "mobile exit" by NOT subscribing any external consumer:
// the only subscriber on the registry is the keepalive fan-in
// goroutine itself. We then publish backend events and assert the
// recordingListener receives Ticks whose lastActivity advances.
func TestRegression_MobileExitDoesNotKillBusyAgent(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithKeepalive(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	first := publishUntilTick(t, svc, rec, 500*time.Millisecond)

	// Pause so a subsequent Bump produces a *later* timestamp on the
	// next Tick; without the pause both Bumps could land within the
	// same millisecond and the assertion below would falsely fail.
	time.Sleep(10 * time.Millisecond)

	second := publishUntilTick(t, svc, rec, 500*time.Millisecond)
	if !second.After(first) {
		t.Fatalf("second tick lastActivity %v should be after first %v", second, first)
	}
}

// TestKeepalive_NoExternalSubscribersStillReceiveTicks pins the core
// invariant: keepalive Ticks fire even when no SSE/HTTP client is
// subscribed. The keepalive's own fan-in goroutine is the only
// subscriber.
func TestKeepalive_NoExternalSubscribersStillReceiveTicks(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithKeepalive(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	svc.subscribers.Broadcast(agent.Event{Type: agent.EventStatusChange, SessionID: "s1"})
	if waitForTickCount(rec, 1, 500*time.Millisecond) == 0 {
		t.Fatal("listener received no Tick despite broadcast event")
	}
}

// TestKeepalive_ShutdownCallsListenerClose pins that graceful shutdown
// releases the provider lease (Listener.Close is invoked).
func TestKeepalive_ShutdownCallsListenerClose(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithKeepalive(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}

	svc.Shutdown()
	if !rec.wasClosed() {
		t.Error("Listener.Close was not called by Shutdown")
	}
}

// TestKeepalive_NilListenerSkipsWiring pins the laptop-mode default:
// when no Listener is configured, no keepalive goroutine starts and
// no subscriber slot is held.
func TestKeepalive_NilListenerSkipsWiring(t *testing.T) {
	t.Parallel()
	svc := New(Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{},
		// KeepaliveListener: nil
	})
	if svc.keepaliveLoop != nil {
		t.Error("keepaliveLoop should be nil when no Listener configured")
	}
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	if got := len(svc.subscribers.subs); got != 0 {
		t.Errorf("subscriber slots = %d, want 0 (no keepalive subscriber)", got)
	}
}

// publishUntilTick broadcasts events into the subscriberRegistry until
// the recordingListener has received a fresh non-zero Tick or the
// timeout elapses. Returns the latest Tick's lastActivity.
func publishUntilTick(t *testing.T, svc *Service, rec *recordingListener, timeout time.Duration) time.Time {
	t.Helper()
	baseline := rec.tickCount()
	deadline := time.After(timeout)
	for {
		svc.subscribers.Broadcast(agent.Event{Type: agent.EventStatusChange, SessionID: "s1"})
		select {
		case <-deadline:
			t.Fatalf("no fresh tick within %v (count stayed at %d)", timeout, baseline)
		case <-time.After(15 * time.Millisecond):
		}
		if rec.tickCount() > baseline {
			ts := rec.lastNonZeroTick()
			if !ts.IsZero() {
				return ts
			}
		}
	}
}

func waitForTickCount(rec *recordingListener, want int, timeout time.Duration) int {
	deadline := time.After(timeout)
	for {
		if c := rec.tickCount(); c >= want {
			return c
		}
		select {
		case <-deadline:
			return rec.tickCount()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
