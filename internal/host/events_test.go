package host

// Unit tests for subscriberRegistry. End-to-end coverage of the same
// concept lives in internal/cli/daemoncli/events_e2e_test.go (where
// the host is wired behind a real gateway and daemonclient). The
// tests here pin the behaviors that are hard to exercise through the
// wire — slow-consumer drop semantics, idempotent unsubscribe, etc.

import (
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

func TestSubscriberRegistry_Subscribe_ReceivesBroadcast(t *testing.T) {
	t.Parallel()
	r := newSubscriberRegistry()
	id, ch := r.Subscribe()
	t.Cleanup(func() { r.Unsubscribe(id) })

	evt := agent.Event{Type: agent.EventStatusChange, SessionID: "s1"}
	r.Broadcast(evt)

	select {
	case got := <-ch:
		if got.Type != agent.EventStatusChange || got.SessionID != "s1" {
			t.Errorf("got %+v, want %+v", got, evt)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("subscriber did not receive broadcast")
	}
}

func TestSubscriberRegistry_MultipleSubscribers_AllReceive(t *testing.T) {
	t.Parallel()
	r := newSubscriberRegistry()
	const n = 5
	chs := make([]<-chan agent.Event, n)
	for i := 0; i < n; i++ {
		id, ch := r.Subscribe()
		chs[i] = ch
		t.Cleanup(func() { r.Unsubscribe(id) })
	}

	evt := agent.Event{Type: agent.EventMessage, SessionID: "fanout"}
	r.Broadcast(evt)

	for i, ch := range chs {
		select {
		case got := <-ch:
			if got.SessionID != "fanout" {
				t.Errorf("subscriber %d got %+v, want fanout", i, got)
			}
		case <-time.After(200 * time.Millisecond):
			t.Errorf("subscriber %d did not receive broadcast", i)
		}
	}
}

// TestSubscriberRegistry_SlowConsumerDoesNotBlockPublisher pins the
// non-blocking-fan-out invariant. Without it, one TUI client that
// stops draining /events would freeze every other client and the
// entire backend-relay goroutine. The bound is fixed at
// eventBufferSize — past that the publisher silently drops.
func TestSubscriberRegistry_SlowConsumerDoesNotBlockPublisher(t *testing.T) {
	t.Parallel()
	r := newSubscriberRegistry()
	id, _ := r.Subscribe() // never drain — simulate a hung TUI
	t.Cleanup(func() { r.Unsubscribe(id) })

	// Push more events than the buffer can hold. If Broadcast were
	// blocking, this would deadlock and time out.
	done := make(chan struct{})
	go func() {
		for i := 0; i < eventBufferSize*4; i++ {
			r.Broadcast(agent.Event{Type: agent.EventStatusChange})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast blocked on slow consumer (publisher must drop, not wait)")
	}
}

// TestSubscriberRegistry_UnsubscribeIdempotent guards against the
// shutdown-race scenario: Service.Shutdown calls CloseAll, then a
// late HTTP handler tries to Unsubscribe. A panic on close of closed
// channel would crash the daemon during a clean exit.
func TestSubscriberRegistry_UnsubscribeIdempotent(t *testing.T) {
	t.Parallel()
	r := newSubscriberRegistry()
	id, _ := r.Subscribe()
	r.Unsubscribe(id)
	r.Unsubscribe(id) // must not panic
}

// TestSubscriberRegistry_CloseAllClosesEverySubscriber ensures the
// shutdown path drains every subscriber's channel. The HTTP /events
// handler ranges over its channel; without close, the goroutine
// leaks past Shutdown.
func TestSubscriberRegistry_CloseAllClosesEverySubscriber(t *testing.T) {
	t.Parallel()
	r := newSubscriberRegistry()
	const n = 3
	chs := make([]<-chan agent.Event, n)
	for i := 0; i < n; i++ {
		_, chs[i] = r.Subscribe()
	}
	r.CloseAll()

	for i, ch := range chs {
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("subscriber %d channel still open after CloseAll", i)
			}
		case <-time.After(200 * time.Millisecond):
			t.Errorf("subscriber %d channel did not close after CloseAll", i)
		}
	}
}

// TestSubscriberRegistry_UnsubscribeDuringBroadcast pins the
// concurrency invariant: a subscriber unsubscribing concurrently
// with a Broadcast must not panic on send-to-closed-channel.
// Broadcast holds RLock and uses a select with default — a racing
// Unsubscribe acquires the write lock and closes the channel after
// the read lock is released. The send branch can't fire after that
// because the map has been mutated. We exercise the race directly
// here so the race detector flags any lock-ordering bug.
func TestSubscriberRegistry_UnsubscribeDuringBroadcast(t *testing.T) {
	t.Parallel()
	r := newSubscriberRegistry()

	const subs = 8
	ids := make([]string, subs)
	for i := 0; i < subs; i++ {
		ids[i], _ = r.Subscribe()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			r.Broadcast(agent.Event{Type: agent.EventStatusChange})
		}
	}()
	go func() {
		defer wg.Done()
		for _, id := range ids {
			r.Unsubscribe(id)
		}
	}()
	wg.Wait()
}
