package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/store"
)

// SessionStoreNotConfigured is returned by session-metadata methods
// when the Service was constructed without Options.SessionsStore.
var SessionStoreNotConfigured = errors.New("host: session store not configured")

// UpsertSessionMetadata persists the session record.
func (s *Service) UpsertSessionMetadata(ctx context.Context, info agent.SessionInfo) error {
	if s.sessionsStore == nil {
		return SessionStoreNotConfigured
	}
	return s.sessionsStore.UpsertSession(ctx, info)
}

// ListSessionMetadata returns every persisted session, newest-updated
// first.
func (s *Service) ListSessionMetadata(ctx context.Context) ([]agent.SessionInfo, error) {
	if s.sessionsStore == nil {
		return nil, SessionStoreNotConfigured
	}
	// TODO(perf-debug): remove once daemon-latency investigation lands.
	// Logs slow ListSessions calls to localize the source of the
	// 0.2-5s stalls observed in the TUI sidebar load path.
	start := time.Now()
	out, err := s.sessionsStore.ListSessions(ctx)
	if elapsed := time.Since(start); elapsed > sessionMetaSlowQueryThreshold {
		s.log.Printf("perf: ListSessions took %s (n=%d, err=%v)", elapsed, len(out), err)
	}
	return out, err
}

// sessionMetaSlowQueryThreshold is the minimum duration above which a
// session-metadata store call is logged. Tuned to surface the stalls
// reported by TUI clients without spamming on warm-pool hits.
const sessionMetaSlowQueryThreshold = 50 * time.Millisecond

// SearchSessionMetadata applies the filters in p and returns matching
// sessions, newest-updated first.
func (s *Service) SearchSessionMetadata(ctx context.Context, p store.SearchParams) ([]agent.SessionInfo, error) {
	if s.sessionsStore == nil {
		return nil, SessionStoreNotConfigured
	}
	return s.sessionsStore.SearchSessions(ctx, p)
}

// GetSessionMetadata returns one persisted session by ID, decorated
// with runtime-only fields from the live backend when one is registered
// (agent-owned session modes for the client's mode picker).
func (s *Service) GetSessionMetadata(ctx context.Context, id string) (agent.SessionInfo, error) {
	if s.sessionsStore == nil {
		return agent.SessionInfo{}, SessionStoreNotConfigured
	}
	info, err := s.sessionsStore.GetSession(ctx, id)
	if errors.Is(err, store.ErrSessionNotFound) {
		return agent.SessionInfo{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return info, err
	}
	if b, ok := s.Session(id); ok {
		if mr, ok := b.(agent.ModeReporter); ok {
			info.CurrentModeID, info.AvailableModes = mr.Modes()
		}
		if mr, ok := b.(agent.ModelReporter); ok {
			info.CurrentModelID, info.AvailableModels = mr.Models()
		}
		if cr, ok := b.(agent.ConfigOptionsReporter); ok {
			info.ConfigOptions = cr.ConfigOptions()
		}
	}
	return info, nil
}

// FindSessionByExternalID looks up a session by the backend-assigned
// id. Used by discovery to dedupe historical sessions on rebuild.
func (s *Service) FindSessionByExternalID(ctx context.Context, externalID string) (agent.SessionInfo, error) {
	if s.sessionsStore == nil {
		return agent.SessionInfo{}, SessionStoreNotConfigured
	}
	info, err := s.sessionsStore.FindSessionByExternalID(ctx, externalID)
	if errors.Is(err, store.ErrSessionNotFound) {
		return agent.SessionInfo{}, fmt.Errorf("external_id %s: %w", externalID, ErrNotFound)
	}
	return info, err
}

// DeleteSessionMetadata removes the persisted session row. Idempotent.
// Note: this does NOT stop a running session backend; callers should
// invoke StopSession first if they want both.
func (s *Service) DeleteSessionMetadata(ctx context.Context, id string) error {
	if s.sessionsStore == nil {
		return SessionStoreNotConfigured
	}
	if err := s.sessionsStore.DeleteSession(ctx, id); err != nil {
		return err
	}
	s.subscribers.Broadcast(agent.Event{
		Type:      agent.EventSessionDelete,
		SessionID: id,
		Timestamp: time.Now(),
	})
	return nil
}

// MarkSessionRead bumps last_read_at on the session record. Returns
// ErrNotFound if the session doesn't exist.
func (s *Service) MarkSessionRead(ctx context.Context, id string) error {
	return s.mutateSessionMeta(ctx, id, func(info *agent.SessionInfo) {
		info.LastReadAt = time.Now()
	})
}

// SetSessionVisibility updates the visibility flag (e.g. archived).
func (s *Service) SetSessionVisibility(ctx context.Context, id string, vis agent.SessionVisibility) error {
	return s.mutateSessionMeta(ctx, id, func(info *agent.SessionInfo) {
		info.Visibility = vis
	})
}

// SetSessionDraft persists an in-progress prompt draft.
func (s *Service) SetSessionDraft(ctx context.Context, id, draft string) error {
	return s.mutateSessionMeta(ctx, id, func(info *agent.SessionInfo) {
		info.Draft = draft
	})
}

// ToggleSessionFollowUp flips the follow_up flag and returns the new
// state. Does NOT bump UpdatedAt — see mutateSessionMeta.
func (s *Service) ToggleSessionFollowUp(ctx context.Context, id string) (agent.SessionInfo, error) {
	if s.sessionsStore == nil {
		return agent.SessionInfo{}, SessionStoreNotConfigured
	}
	info, err := s.sessionsStore.GetSession(ctx, id)
	if errors.Is(err, store.ErrSessionNotFound) {
		return agent.SessionInfo{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return agent.SessionInfo{}, err
	}
	info.FollowUp = !info.FollowUp
	if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
		return agent.SessionInfo{}, err
	}
	s.broadcastMetaChange(info)
	return info, nil
}

// mutateSessionMeta is the read-modify-write helper for the setters
// above. Does NOT bump UpdatedAt — that field tracks agent-driven
// activity, so user metadata changes (mark-read, follow-up, draft)
// stay invisible to the inbox's newest-first sort.
func (s *Service) mutateSessionMeta(ctx context.Context, id string, mutate func(*agent.SessionInfo)) error {
	if s.sessionsStore == nil {
		return SessionStoreNotConfigured
	}
	info, err := s.sessionsStore.GetSession(ctx, id)
	if errors.Is(err, store.ErrSessionNotFound) {
		return fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return err
	}
	mutate(&info)
	if err := s.sessionsStore.UpsertSession(ctx, info); err != nil {
		return err
	}
	s.broadcastMetaChange(info)
	return nil
}

// broadcastMetaChange fans out an EventMetaChange to all subscribers
// with the post-mutation SessionInfo. Subscribers replace their cached
// row wholesale instead of diffing per-field changes.
//
// Every code path that persists a SessionInfo mutation should funnel
// through this helper so the sidebar — which sorts/redraws off the
// post-mutation row — never lags the DB. The two call sites are
// mutateSessionMeta (user-driven: mark-read, draft, follow-up,
// visibility) and applyEventToMetadata (backend-driven: status flip,
// title change, ExternalID stamp).
func (s *Service) broadcastMetaChange(info agent.SessionInfo) {
	s.subscribers.Broadcast(agent.Event{
		Type:       agent.EventMetaChange,
		SessionID:  info.ID,
		ExternalID: info.ExternalID,
		Timestamp:  time.Now(),
		Data:       agent.MetaChangeData{Session: info},
	})
}

// Subscribe registers an event subscriber and returns an opaque ID
// and the receive channel. Caller must Unsubscribe when done. Slow
// consumers drop events instead of blocking the publisher.
func (s *Service) Subscribe() (string, <-chan agent.Event) {
	return s.subscribers.Subscribe()
}

// Unsubscribe deregisters the given subscriber and closes its channel.
// Idempotent.
func (s *Service) Unsubscribe(id string) {
	s.subscribers.Unsubscribe(id)
}
