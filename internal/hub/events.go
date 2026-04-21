package hub

import "github.com/acksell/clank/internal/agent"

// broadcast sends an event to all connected subscribers. Slow
// subscribers drop events rather than block the publisher.
func (s *Service) broadcast(evt agent.Event) {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	for _, ch := range s.subscribers {
		select {
		case ch <- evt:
		default:
		}
	}
}
