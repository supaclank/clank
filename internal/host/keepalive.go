package host

import (
	"context"
	"time"
)

// startKeepalive subscribes to the subscriber registry and forwards
// every backend event into the keepalive Loop, then starts the Loop's
// goroutine. No-op when KeepaliveListener wasn't set.
//
// The fan-in goroutine is intentionally tight (range eventCh → Bump
// → continue) so its only failure mode is "fell behind during a burst,
// dropped a few events" — Bump is itself non-blocking, and the Loop's
// internal channel buffer coalesces. Any surviving event in the burst
// refreshes lastActivity.
func (s *Service) startKeepalive() {
	if s.keepaliveLoop == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.keepaliveStop = cancel

	subID, eventCh := s.subscribers.Subscribe()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.subscribers.Unsubscribe(subID)
		for range eventCh {
			s.keepaliveLoop.Bump()
		}
	}()
	go s.keepaliveLoop.Run(ctx)
}

// stopKeepalive halts the Loop and releases the provider lease.
// Called from Shutdown after subscribers.CloseAll() has drained the
// fan-in goroutine.
func (s *Service) stopKeepalive() {
	if s.keepaliveLoop == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.keepaliveLoop.Stop(stopCtx); err != nil {
		s.log.Printf("keepalive: stop: %v", err)
	}
	if s.keepaliveStop != nil {
		s.keepaliveStop()
	}
}
