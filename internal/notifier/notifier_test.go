package notifier

import (
	"context"
	"sync"
	"testing"
	"time"
)

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

func (p *recordingProvider) sentSnapshot() []Notification {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Notification, len(p.sent))
	copy(out, p.sent)
	return out
}

func (p *recordingProvider) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

func TestLoop_DeliversNotificationsInOrder(t *testing.T) {
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

	loop.Notify(Notification{SessionID: "s1", Kind: KindIdle, Title: "Fix login retry", Body: "Done."})
	loop.Notify(Notification{SessionID: "s2", Kind: KindPermission, Title: "Permission requested: bash"})

	if got := waitForSent(rec, 2, 500*time.Millisecond); got != 2 {
		t.Fatalf("got %d notifications, want 2", got)
	}
	sent := rec.sentSnapshot()
	if sent[0].SessionID != "s1" || sent[1].SessionID != "s2" {
		t.Errorf("delivery order = [%s, %s], want [s1, s2]", sent[0].SessionID, sent[1].SessionID)
	}
	if sent[0].Title != "Fix login retry" || sent[0].Body != "Done." {
		t.Errorf("notification delivered altered: %+v (the Loop must pass copy through untouched)", sent[0])
	}
}

// blockingProvider pauses Send until release is called, then records
// the notification. Used to deterministically queue notifications in
// the Loop's input channel before signalling Stop, which is what
// exercises the drain path.
type blockingProvider struct {
	released chan struct{}
	mu       sync.Mutex
	sent     []Notification
	closes   int
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{released: make(chan struct{})}
}

func (p *blockingProvider) release() { close(p.released) }

func (p *blockingProvider) Send(ctx context.Context, n Notification) error {
	select {
	case <-p.released:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	p.sent = append(p.sent, n)
	p.mu.Unlock()
	return nil
}

func (p *blockingProvider) Close(_ context.Context) error {
	p.mu.Lock()
	p.closes++
	p.mu.Unlock()
	return nil
}

func (p *blockingProvider) sentCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

// TestLoop_StopDrainsQueuedNotifications pins the regression: before
// the drain fix, Run returned immediately on l.stop and dropped any
// notifications still sitting in l.queue. With the drain in place the
// shutdown path delivers everything that was already enqueued.
func TestLoop_StopDrainsQueuedNotifications(t *testing.T) {
	t.Parallel()
	rec := newBlockingProvider()
	loop := New(Config{Provider: rec, Buffer: 4})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// Three notifications buffered while the provider is still blocked.
	// None are delivered yet.
	for i := 0; i < 3; i++ {
		loop.Notify(Notification{SessionID: "s1", Kind: KindIdle})
	}
	// Give the worker time to consume the first notification into
	// send() — that one's blocked on the provider. The remaining 2 sit
	// in l.queue.
	time.Sleep(20 * time.Millisecond)

	// Release the provider AFTER Stop has been signalled so the
	// blocked Send completes, then the drain runs through the
	// remaining queued notifications. Stop blocks until done is closed.
	done := make(chan error, 1)
	go func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		done <- loop.Stop(stopCtx)
	}()
	time.Sleep(20 * time.Millisecond)
	rec.release()

	if err := <-done; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := rec.sentCount(); got != 3 {
		t.Errorf("Provider.Send call count = %d, want 3 (drain should deliver every queued notification)", got)
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
