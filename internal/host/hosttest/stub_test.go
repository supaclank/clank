package hosttest

import (
	"sync"
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// TestStubBackend_PushEventStopRace pins the PushEvent/Stop race: Stop
// must never close events while a PushEvent that passed its preflight
// done-check is still able to send on it. Run with -race.
func TestStubBackend_PushEventStopRace(t *testing.T) {
	for i := 0; i < 2000; i++ {
		b := &StubBackend{
			events: make(chan agent.Event, 1),
			done:   make(chan struct{}),
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.PushEvent(agent.Event{})
		}()
		go func() {
			defer wg.Done()
			b.Stop()
		}()
		wg.Wait()
	}
}
