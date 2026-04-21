package hub

// HUB
// persistSession writes the session to the store if persistence is enabled.
// Must be called while s.mu is held (read or write lock).
func (s *Service) persistSession(ms *managedSession) {
	if s.Store == nil {
		return
	}
	if err := s.Store.UpsertSession(ms.info); err != nil {
		s.log.Printf("warning: persist session %s: %v", ms.info.ID, err)
	}
}

// HUB
// deletePersistedSession removes the session from the store if persistence is enabled.
func (s *Service) deletePersistedSession(id string) {
	if s.Store == nil {
		return
	}
	if err := s.Store.DeleteSession(id); err != nil {
		s.log.Printf("warning: delete persisted session %s: %v", id, err)
	}
}
