package host

// Session-viewer presence. A "viewer" is a client that declared a human
// is currently looking at a session — the per-session SSE handler
// registers one for each stream opened with ?viewer=1 (chat-client-spec
// §events). The notifier uses presence to suppress "agent finished"
// pushes for sessions the user is already watching (the guest-app
// overlay prompt box, the mobile session view).
//
// Opt-in on purpose: the same SSE endpoint is also tailed by machine
// consumers (the hub's relay client mirroring a remote session), and a
// misclassified machine "viewer" would silently swallow notifications.
// An unmarked stream keeps today's behavior — pushes flow.

// AddSessionViewer registers a live viewer of sessionID and returns a
// release func. Callers must invoke release exactly once when the
// viewer's stream closes; it is not idempotent.
func (s *Service) AddSessionViewer(sessionID string) (release func()) {
	s.viewersMu.Lock()
	s.viewers[sessionID]++
	s.viewersMu.Unlock()
	return func() {
		s.viewersMu.Lock()
		defer s.viewersMu.Unlock()
		if s.viewers[sessionID] <= 1 {
			delete(s.viewers, sessionID)
			return
		}
		s.viewers[sessionID]--
	}
}

// SessionHasViewers reports whether at least one viewer stream is
// currently open for sessionID.
func (s *Service) SessionHasViewers(sessionID string) bool {
	s.viewersMu.Lock()
	defer s.viewersMu.Unlock()
	return s.viewers[sessionID] > 0
}
