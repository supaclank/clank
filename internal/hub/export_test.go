package hub

import "github.com/acksell/clank/internal/agent"

// InjectSession exposes injectSession for external tests (package hub_test).
func (s *Service) InjectSession(info agent.SessionInfo) { s.injectSession(info) }

// injectSession adds a session to the hub's in-memory map without starting
// a backend. Used by tests via the export_test.go bridge.
func (s *Service) injectSession(info agent.SessionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[info.ID] = &managedSession{info: info}
}
