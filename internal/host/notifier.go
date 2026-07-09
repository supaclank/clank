package host

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/internal/notifier"
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

// sessionNotificationContext resolves the display metadata the notifier
// stamps onto outgoing pushes: the session title from the sessions store
// and the agent's latest reply from the backend transcript. Best-effort —
// a failed lookup degrades the notification copy, never blocks delivery.
func (s *Service) sessionNotificationContext(ctx context.Context, sessionID string) notifier.SessionContext {
	var out notifier.SessionContext
	if s.sessionsStore != nil {
		info, err := s.sessionsStore.GetSession(ctx, sessionID)
		switch {
		case err == nil:
			out.Title = info.Title
		case !errors.Is(err, store.ErrSessionNotFound) && !isExpiredContext(err):
			s.log.Printf("notifier: load session %s metadata: %v", sessionID, err)
		}
	}
	b, ok := s.Session(sessionID)
	if !ok {
		return out
	}
	msgs, err := b.Messages(ctx)
	if err != nil {
		if !isExpiredContext(err) {
			s.log.Printf("notifier: read transcript for session %s: %v", sessionID, err)
		}
		return out
	}
	out.LastAssistantText = lastAssistantText(msgs)
	return out
}

// isExpiredContext reports whether err is the sendCtx deadline or a
// shutdown cancellation rather than a genuine lookup failure — expected
// noise on every shutdown/slow-lookup, not worth logging.
func isExpiredContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// lastAssistantText returns the text of the newest assistant message
// that has any, walking past tool-only tail messages.
func lastAssistantText(msgs []agent.MessageData) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		if t := messageText(msgs[i]); t != "" {
			return t
		}
	}
	return ""
}

// messageText concatenates a message's text parts; Content covers
// backends that don't populate parts.
func messageText(m agent.MessageData) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type != agent.PartText || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	if b.Len() == 0 {
		return m.Content
	}
	return b.String()
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
