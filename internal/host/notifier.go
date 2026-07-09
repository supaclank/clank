package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/internal/notifier"
)

// pushContextTimeout bounds the per-notification session-metadata
// lookup (store read + transcript read) so a wedged backend can't
// stall the fan-in goroutine past a burst.
const pushContextTimeout = 3 * time.Second

// startNotifier subscribes to the subscriber registry, classifies each
// backend event, and hands push-worthy ones — enriched with session
// metadata — to the notifier Loop for delivery. The Loop is a dumb
// pipeline; which events notify and what they say is decided here,
// where the Service can query its own store and backends. No-op when
// NotifierLoop wasn't set.
//
// The fan-in goroutine does real work only for push-worthy events
// (idle/permission/error — at most a few per agent turn); everything
// else is discarded by the cheap pure classify. A slow metadata lookup
// therefore risks only this subscriber's own 256-event buffer, the
// same "fell behind during a burst, dropped a few events" failure mode
// a tight relay would have. Broadcast never blocks on us.
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
			n, ok := classifyEvent(evt, pushContext{})
			if !ok {
				continue
			}
			// A watched session doesn't buzz: the finish is already on
			// screen (guest-app overlay, mobile session view). Permission
			// and error pushes still go out — missing one because a stale
			// viewer socket lingered would leave the agent blocked.
			if n.Kind == notifier.KindIdle && s.SessionHasViewers(evt.SessionID) {
				s.log.Printf("notifier: suppressed idle push for viewed session %s", evt.SessionID)
				continue
			}
			// Re-classifying with the fetched context keeps all copy
			// decisions inside classifyEvent.
			lookupCtx, lookupCancel := context.WithTimeout(ctx, pushContextTimeout)
			if pctx := s.pushContextFor(lookupCtx, evt.SessionID); pctx != (pushContext{}) {
				n, _ = classifyEvent(evt, pctx)
			}
			lookupCancel()
			s.notifierLoop.Notify(n)
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

// pushContext is display metadata for the session a notification is
// about, resolved at notification time. Zero values mean "unknown" and
// the notification keeps its generic copy.
type pushContext struct {
	Title             string // Session title (AI-generated or user-set)
	LastAssistantText string // Plain text of the agent's latest reply
}

// pushContextFor resolves the display metadata stamped onto outgoing
// pushes: the session title from the sessions store and the agent's
// latest reply from the backend transcript. Best-effort — a failed
// lookup degrades the notification copy, never blocks delivery.
func (s *Service) pushContextFor(ctx context.Context, sessionID string) pushContext {
	var out pushContext
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

// isExpiredContext reports whether err is the lookup deadline or a
// shutdown cancellation rather than a genuine lookup failure — expected
// noise on every shutdown/slow-lookup, not worth logging.
func isExpiredContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// classifyEvent decides whether evt should produce a push and builds
// its copy. Pure function — no Service access, no clock other than the
// event's own Timestamp — so it's unit-testable in isolation. This is
// the host's push-worthiness policy; a future per-user notification
// settings layer plugs in here.
//
// Mappings:
//   - EventStatusChange to StatusIdle from StatusBusy → KindIdle.
//     Busy→Idle is the only Idle transition we treat as notification-
//     worthy (this includes a user-initiated Abort, see
//     ClaudeCodeBackend.handleResult — reaching Idle from Busy is enough,
//     regardless of how the turn ended). The create path goes
//     Starting→Busy→Idle, and OpenCode never sits in Starting at all.
//     Everything else reaching Idle is ignored —
//     idle→idle (daemon-restart normalization, see
//     normalizeStaleSessionStatus) and starting→idle (the re-attach
//     handshake when Open rehydrates an existing session, e.g. the user
//     opening an old session on the phone) would push spurious "agent
//     finished".
//   - EventPermission → KindPermission. Data carries request_id so the
//     mobile client can prefill the approval UI on deep-link.
//   - EventError → KindError.
//   - Everything else: dropped (message/part/title/voice/etc. are too
//     chatty for push).
//
// Copy layout follows the messaging-app convention: when pctx names the
// session, the title is the session name and what-happened moves into
// the body; with a zero pctx the kind-generic copy is used. Data always
// carries session_title when known so clients can render it regardless.
func classifyEvent(evt agent.Event, pctx pushContext) (notifier.Notification, bool) {
	when := evt.Timestamp
	if when.IsZero() {
		when = time.Now()
	}
	switch evt.Type {
	case agent.EventStatusChange:
		d, ok := evt.Data.(agent.StatusChangeData)
		if !ok {
			return notifier.Notification{}, false
		}
		if d.NewStatus != agent.StatusIdle {
			return notifier.Notification{}, false
		}
		if d.OldStatus != agent.StatusBusy {
			return notifier.Notification{}, false
		}
		title := "Agent finished"
		body := "Tap to see the result."
		if pctx.Title != "" {
			title = truncateRunes(pctx.Title, maxTitleLen)
			body = "Finished — tap to see the result."
		}
		if preview := previewText(pctx.LastAssistantText); preview != "" {
			body = preview
		}
		return notifier.Notification{
			SessionID:  evt.SessionID,
			Kind:       notifier.KindIdle,
			Title:      title,
			Body:       body,
			Data:       sessionTitleData(nil, pctx),
			OccurredAt: when,
		}, true
	case agent.EventPermission:
		d, ok := evt.Data.(agent.PermissionData)
		if !ok {
			return notifier.Notification{}, false
		}
		title := "Permission requested"
		if d.Tool != "" {
			title = fmt.Sprintf("Permission requested: %s", d.Tool)
		}
		body := d.Description
		if body == "" {
			body = "Tap to review and approve."
		}
		if pctx.Title != "" {
			body = fmt.Sprintf("%s — %s", title, body)
			title = truncateRunes(pctx.Title, maxTitleLen)
		}
		return notifier.Notification{
			SessionID:  evt.SessionID,
			Kind:       notifier.KindPermission,
			Title:      title,
			Body:       body,
			Data:       sessionTitleData(map[string]any{"request_id": d.RequestID, "tool": d.Tool}, pctx),
			OccurredAt: when,
		}, true
	case agent.EventError:
		d, ok := evt.Data.(agent.ErrorData)
		if !ok {
			return notifier.Notification{}, false
		}
		title := "Agent error"
		body := d.Message
		if body == "" {
			body = "Tap to see details."
		}
		if pctx.Title != "" {
			body = fmt.Sprintf("Agent error — %s", body)
			title = truncateRunes(pctx.Title, maxTitleLen)
		}
		return notifier.Notification{
			SessionID:  evt.SessionID,
			Kind:       notifier.KindError,
			Title:      title,
			Body:       body,
			Data:       sessionTitleData(nil, pctx),
			OccurredAt: when,
		}, true
	default:
		return notifier.Notification{}, false
	}
}

// sessionTitleData stamps the session title into a notification's Data
// map when known, allocating the map only if needed.
func sessionTitleData(data map[string]any, pctx pushContext) map[string]any {
	if pctx.Title == "" {
		return data
	}
	if data == nil {
		data = make(map[string]any, 1)
	}
	data["session_title"] = truncateRunes(pctx.Title, maxTitleLen)
	return data
}

// maxTitleLen bounds the session title used as notification Title and
// Data.session_title. Nothing upstream bounds AI-generated or user-set
// session titles; push providers reject oversized payloads.
const maxTitleLen = 80

// truncateRunes caps s to max runes, appending an ellipsis when cut.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// maxBodyPreviewLen caps the last-reply preview in a notification body.
// Push banners show ~2 lines; OSes truncate the rest anyway, and Expo
// rejects payloads over 4 KiB.
const maxBodyPreviewLen = 180

// previewScanBytes bounds how much of s previewText inspects before
// flattening whitespace. Collapsing whitespace only ever shortens the
// result, so scanning a generous prefix instead of all of s can't change
// the final maxBodyPreviewLen-rune output, but keeps a huge tool-output
// reply from costing memory/CPU proportional to its full length.
const previewScanBytes = maxBodyPreviewLen * 4

// previewText flattens s into a single-line preview: whitespace runs
// (including newlines) collapse to one space, then the result is
// truncated to maxBodyPreviewLen runes with an ellipsis.
func previewText(s string) string {
	if len(s) > previewScanBytes {
		s = strings.ToValidUTF8(s[:previewScanBytes], "")
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	out := strings.Join(fields, " ")
	if r := []rune(out); len(r) > maxBodyPreviewLen {
		out = string(r[:maxBodyPreviewLen-1]) + "…"
	}
	return out
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
