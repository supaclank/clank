// Package notifier turns the host's backend-event stream into outbound
// Notifications via a provider-specific Provider. It is a parallel
// consumer of the host's subscriberRegistry: keepalive coalesces every
// event into a single Bump because it only cares about "is there
// activity", whereas the notifier inspects each event because it has
// to decide what kind of notification to deliver (idle, permission,
// error, …).
//
// The Loop owns an internal buffered channel + worker goroutine: the
// host's subscriber-fanin calls OnEvent (non-blocking), and the worker
// drains it serially into Provider.Send. Send is allowed to be slow —
// the worker isolates it from the event publisher so a misbehaving
// provider can't backpressure the host.
package notifier

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// Kind classifies a Notification. Providers may use Kind to choose
// transport (e.g. high-priority push for KindPermission) or copy.
type Kind string

const (
	KindIdle       Kind = "idle"       // Agent transitioned to idle (finished a turn)
	KindPermission Kind = "permission" // Agent is waiting for tool permission
	KindError      Kind = "error"      // Agent encountered an error
)

// Notification is the canonical, provider-agnostic shape. Providers
// translate this into their delivery format (Expo Push payload,
// webhook body, …). SessionID is opaque metadata — the receiver uses
// it to deep-link, not to authenticate.
type Notification struct {
	SessionID  string         `json:"session_id"`
	Kind       Kind           `json:"kind"`
	Title      string         `json:"title"`
	Body       string         `json:"body"`
	Data       map[string]any `json:"data,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
}

// SessionContext is display metadata for the session a notification is
// about, resolved at delivery time. Zero values mean "unknown" and the
// notification keeps its generic copy.
type SessionContext struct {
	Title             string // Session title (AI-generated or user-set)
	LastAssistantText string // Plain text of the agent's latest reply
}

// SessionContextFunc resolves SessionContext for a session. Called from
// the Loop's worker goroutine only for notification-worthy events, under
// the same timeout budget as Provider.Send. Implementations must treat
// failures as "unknown" (return the zero value), never block delivery.
type SessionContextFunc func(ctx context.Context, sessionID string) SessionContext

// Provider delivers Notifications to the outside world. Implementations
// live under internal/notifier/<name>/. Send is called serially from
// the Loop's worker goroutine — no internal locking required, but it
// should respect ctx for cancellation.
type Provider interface {
	Send(ctx context.Context, n Notification) error
	Close(ctx context.Context) error
}

const (
	// DefaultBuffer is the worker's input queue size. Sized for typical
	// burstiness — a few permission requests + status changes during a
	// single agent turn. Overflow drops events (and logs) rather than
	// blocking the host's subscriber-fanin.
	DefaultBuffer = 64

	// DefaultSendTimeout caps a single Provider.Send. Picked to be
	// short enough that a hung provider doesn't drain the buffer to
	// drops while still leaving room for normal HTTP round-trips.
	DefaultSendTimeout = 5 * time.Second
)

// Config configures a Loop at construction time. Provider is required.
type Config struct {
	Provider    Provider
	Log         *log.Logger
	Buffer      int
	SendTimeout time.Duration
}

// Loop is the event-driven notifier. Construct with New, drive events
// in via OnEvent, run the worker with Run, stop with Stop.
type Loop struct {
	provider       Provider
	log            *log.Logger
	sendTimeout    time.Duration
	sessionContext SessionContextFunc

	events chan agent.Event
	stop   chan struct{}
	done   chan struct{}

	stopOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
}

// New constructs a Loop. Panics on missing Provider — fast failure
// beats a later nil deref.
func New(cfg Config) *Loop {
	if cfg.Provider == nil {
		panic("notifier.New: Provider is required")
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultBuffer
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = DefaultSendTimeout
	}
	if cfg.Log == nil {
		cfg.Log = log.New(os.Stderr, "[notifier] ", log.LstdFlags|log.Lmsgprefix)
	}
	return &Loop{
		provider:    cfg.Provider,
		log:         cfg.Log,
		sendTimeout: cfg.SendTimeout,
		events:      make(chan agent.Event, cfg.Buffer),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// SetSessionContext installs the lookup used to enrich notifications
// with session display metadata (title, last assistant reply). Nil
// keeps the generic copy. Must be called before Run — the worker reads
// the field unlocked.
func (l *Loop) SetSessionContext(f SessionContextFunc) { l.sessionContext = f }

// OnEvent enqueues evt for asynchronous classification + delivery.
// Non-blocking: if the worker's input queue is full, evt is dropped
// and logged. Safe from any goroutine.
//
// We drop rather than block because the caller is the host's
// subscriber-fanin goroutine — blocking it would back up every other
// subscriber (SSE handlers, keepalive). For a permission/idle event
// we'd rather lose a notification than starve the rest of the system.
func (l *Loop) OnEvent(evt agent.Event) {
	select {
	case l.events <- evt:
	default:
		l.log.Printf("input buffer full; dropping %s event for session %s", evt.Type, evt.SessionID)
	}
}

// Run drives the worker until ctx is canceled or Stop is called.
// Drains events from the input queue and hands notification-worthy
// ones to Provider.Send. Logs (and continues) on Send error — retries
// and DLQs are the Provider's responsibility.
//
// On Stop the worker drains whatever's already queued before exiting
// so the last permission/idle/error notifications aren't lost during
// graceful shutdown. On ctx cancellation we exit immediately — that
// path is for hard teardown (the parent gave up waiting).
func (l *Loop) Run(ctx context.Context) {
	defer close(l.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stop:
			l.drainAndExit(ctx)
			return
		case evt := <-l.events:
			l.handle(ctx, evt)
		}
	}
}

// drainAndExit empties l.events under the ambient ctx. handle()
// applies its own SendTimeout, so an unresponsive provider doesn't
// extend shutdown indefinitely — it bounds each remaining event.
func (l *Loop) drainAndExit(ctx context.Context) {
	for {
		select {
		case evt := <-l.events:
			l.handle(ctx, evt)
		default:
			return
		}
	}
}

func (l *Loop) handle(ctx context.Context, evt agent.Event) {
	n, ok := classify(evt, SessionContext{})
	if !ok {
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, l.sendTimeout)
	defer cancel()
	// The context lookup runs only for notification-worthy events (the
	// zero-context classify above is the filter) and shares the send
	// timeout budget. Re-classifying keeps all copy decisions in one place.
	if l.sessionContext != nil {
		if sctx := l.sessionContext(sendCtx, evt.SessionID); sctx != (SessionContext{}) {
			n, _ = classify(evt, sctx)
		}
	}
	if err := l.provider.Send(sendCtx, n); err != nil {
		l.log.Printf("provider send %s for session %s: %v", n.Kind, n.SessionID, err)
	}
}

// Stop halts Run and calls Provider.Close. Safe to call multiple times
// concurrently: close(l.stop) and Provider.Close each fire exactly
// once; subsequent calls return the cached error from the first Close.
// If ctx fires before Run exits, Stop returns ctx.Err() without
// invoking Provider.Close — a subsequent Stop with a live ctx will
// still close.
func (l *Loop) Stop(ctx context.Context) error {
	l.stopOnce.Do(func() { close(l.stop) })
	select {
	case <-l.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	l.closeOnce.Do(func() {
		l.closeErr = l.provider.Close(ctx)
	})
	return l.closeErr
}

// classify decides whether evt should produce a Notification and builds
// its copy. Pure function — no Provider calls, no clock other than the
// event's own Timestamp — so it's unit-testable in isolation.
//
// Mappings:
//   - EventStatusChange to StatusIdle from StatusBusy/StatusStarting
//     → KindIdle. We deliberately ignore idle→idle (daemon-restart
//     normalization, see host.normalizeStaleSessionStatus) and other
//     non-busy→idle transitions to avoid spurious "agent finished"
//     pushes.
//   - EventPermission → KindPermission. Data carries request_id so the
//     mobile client can prefill the approval UI on deep-link.
//   - EventError → KindError.
//   - Everything else: dropped (message/part/title/voice/etc. are too
//     chatty for push).
//
// Copy layout follows the messaging-app convention: when sctx names the
// session, the title is the session name and what-happened moves into
// the body; with a zero sctx the kind-generic copy is used. Data always
// carries session_title when known so clients can render it regardless.
func classify(evt agent.Event, sctx SessionContext) (Notification, bool) {
	when := evt.Timestamp
	if when.IsZero() {
		when = time.Now()
	}
	switch evt.Type {
	case agent.EventStatusChange:
		d, ok := evt.Data.(agent.StatusChangeData)
		if !ok {
			return Notification{}, false
		}
		if d.NewStatus != agent.StatusIdle {
			return Notification{}, false
		}
		if d.OldStatus != agent.StatusBusy && d.OldStatus != agent.StatusStarting {
			return Notification{}, false
		}
		title := "Agent finished"
		body := "Tap to see the result."
		if sctx.Title != "" {
			title = sctx.Title
			body = "Finished — tap to see the result."
		}
		if preview := previewText(sctx.LastAssistantText); preview != "" {
			body = preview
		}
		return Notification{
			SessionID:  evt.SessionID,
			Kind:       KindIdle,
			Title:      title,
			Body:       body,
			Data:       sessionTitleData(nil, sctx),
			OccurredAt: when,
		}, true
	case agent.EventPermission:
		d, ok := evt.Data.(agent.PermissionData)
		if !ok {
			return Notification{}, false
		}
		title := "Permission requested"
		if d.Tool != "" {
			title = fmt.Sprintf("Permission requested: %s", d.Tool)
		}
		body := d.Description
		if body == "" {
			body = "Tap to review and approve."
		}
		if sctx.Title != "" {
			body = fmt.Sprintf("%s — %s", title, body)
			title = sctx.Title
		}
		return Notification{
			SessionID:  evt.SessionID,
			Kind:       KindPermission,
			Title:      title,
			Body:       body,
			Data:       sessionTitleData(map[string]any{"request_id": d.RequestID, "tool": d.Tool}, sctx),
			OccurredAt: when,
		}, true
	case agent.EventError:
		d, ok := evt.Data.(agent.ErrorData)
		if !ok {
			return Notification{}, false
		}
		title := "Agent error"
		body := d.Message
		if body == "" {
			body = "Tap to see details."
		}
		if sctx.Title != "" {
			body = fmt.Sprintf("Agent error — %s", body)
			title = sctx.Title
		}
		return Notification{
			SessionID:  evt.SessionID,
			Kind:       KindError,
			Title:      title,
			Body:       body,
			Data:       sessionTitleData(nil, sctx),
			OccurredAt: when,
		}, true
	default:
		return Notification{}, false
	}
}

// sessionTitleData stamps the session title into a notification's Data
// map when known, allocating the map only if needed.
func sessionTitleData(data map[string]any, sctx SessionContext) map[string]any {
	if sctx.Title == "" {
		return data
	}
	if data == nil {
		data = make(map[string]any, 1)
	}
	data["session_title"] = sctx.Title
	return data
}

// maxBodyPreviewLen caps the last-reply preview in a notification body.
// Push banners show ~2 lines; OSes truncate the rest anyway, and Expo
// rejects payloads over 4 KiB.
const maxBodyPreviewLen = 180

// previewText flattens s into a single-line preview: whitespace runs
// (including newlines) collapse to one space, then the result is
// truncated to maxBodyPreviewLen runes with an ellipsis.
func previewText(s string) string {
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
