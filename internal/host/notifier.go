package host

import (
	"context"
	"time"
)

// startNotifier subscribes to the subscriber registry and forwards
// every backend event into the notifier Loop, then starts the Loop's
// worker goroutine. No-op when NotifierLoop wasn't set.
//
// Mirrors startKeepalive: a tight fan-in goroutine (range eventCh →
// OnEvent → continue) so its only failure mode is "fell behind during
// a burst, dropped a few events". OnEvent is itself non-blocking, and
// the Loop's internal buffer absorbs short bursts. We deliberately
// don't share the keepalive subscription — the keepalive coalesces
// every event into a single Bump (no event content), whereas the
// notifier needs each event's payload, so the two consumers diverge
// at the very next step and are simpler as independent subscribers.
func (s *Service) startNotifier() {
	if s.notifierLoop == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.notifierStop = cancel

	subID, eventCh := s.subscribers.Subscribe()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.subscribers.Unsubscribe(subID)
		for evt := range eventCh {
			s.notifierLoop.OnEvent(evt)
		}
	}()
	go s.notifierLoop.Run(ctx)
}

// stopNotifier halts the Loop and releases the Provider. Called from
// Shutdown after subscribers.CloseAll() has drained the fan-in
// goroutine. Bounded by a 2s deadline so a misbehaving Provider can't
// hang shutdown.
func (s *Service) stopNotifier() {
	if s.notifierLoop == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.notifierLoop.Stop(stopCtx); err != nil {
		s.log.Printf("notifier: stop: %v", err)
	}
	if s.notifierStop != nil {
		s.notifierStop()
	}
}
