package host

import (
	"sync"

	"github.com/acksell/clank/internal/agent"
	"github.com/oklog/ulid/v2"
)

// eventBufferSize is per-subscriber. Sized for typical agent event
// bursts (a few hundred messages during a single tool-use); slow
// consumers drop events rather than block the publisher.
const eventBufferSize = 256

// subscriberRegistry holds the subscribers that listen for fanned-out
// agent events on the host. Mirrors the hub-side pattern from PR <3.
type subscriberRegistry struct {
	mu   sync.RWMutex
	subs map[string]chan agent.Event
}

func newSubscriberRegistry() *subscriberRegistry {
	return &subscriberRegistry{subs: map[string]chan agent.Event{}}
}

// Subscribe returns a new subscriber id and a buffered receive channel.
// Caller must invoke Unsubscribe(id) when done; channel is closed at
// that point. Channel sends use a default branch — slow subscribers
// drop events instead of blocking the publisher.
func (r *subscriberRegistry) Subscribe() (string, <-chan agent.Event) {
	id := ulid.Make().String()
	ch := make(chan agent.Event, eventBufferSize)
	r.mu.Lock()
	r.subs[id] = ch
	r.mu.Unlock()
	return id, ch
}

// Unsubscribe removes the subscriber and closes its channel. Idempotent.
func (r *subscriberRegistry) Unsubscribe(id string) {
	r.mu.Lock()
	ch, ok := r.subs[id]
	if ok {
		delete(r.subs, id)
	}
	r.mu.Unlock()
	if ok {
		close(ch)
	}
}

// Broadcast sends evt to every current subscriber. Slow subscribers
// drop events (their buffer is full) rather than block this call.
func (r *subscriberRegistry) Broadcast(evt agent.Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, ch := range r.subs {
		select {
		case ch <- evt:
		default:
		}
	}
}

// CloseAll drops every subscriber and closes its channel. Called from
// Service.Shutdown.
func (r *subscriberRegistry) CloseAll() {
	r.mu.Lock()
	subs := r.subs
	r.subs = map[string]chan agent.Event{}
	r.mu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
}
