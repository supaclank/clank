// Package notifier is the host's outbound-notification delivery
// pipeline. It owns queuing, send timeouts, and Provider lifecycle —
// nothing else. Deciding what is push-worthy and composing the copy is
// entirely the caller's job (internal/host classifies backend events
// and enriches them with session metadata before handing over a
// finished Notification); this package never inspects events and has
// no policy of its own.
//
// The Loop owns an internal buffered channel + worker goroutine: the
// caller enqueues via Notify (non-blocking), and the worker drains it
// serially into Provider.Send. Send is allowed to be slow — the worker
// isolates it from the caller so a misbehaving provider can't
// backpressure the host's event fan-out.
package notifier

import (
	"context"
	"log"
	"os"
	"sync"
	"time"
)

// Kind classifies a Notification. Providers may use Kind to choose
// transport (e.g. high-priority push for KindPermission) or copy.
type Kind string

const (
	KindIdle       Kind = "idle"       // Agent transitioned to idle (finished a turn)
	KindPermission Kind = "permission" // Agent is waiting for tool permission
	KindError      Kind = "error"      // Agent encountered an error
	KindCrashed    Kind = "crashed"    // Agent process died mid-turn (no result) — resumable
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

// Provider delivers Notifications to the outside world. Implementations
// live under internal/notifier/<name>/. Send is called serially from
// the Loop's worker goroutine — no internal locking required, but it
// should respect ctx for cancellation.
type Provider interface {
	Send(ctx context.Context, n Notification) error
	Close(ctx context.Context) error
}

const (
	// DefaultBuffer is the worker's input queue size. Notifications are
	// rare (a permission ask or an idle flip per agent turn), so this
	// mostly absorbs a provider that stalls across a burst of sessions.
	// Overflow drops notifications (and logs) rather than blocking the
	// caller.
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

// Loop is the delivery worker. Construct with New, enqueue via Notify,
// run the worker with Run, stop with Stop.
type Loop struct {
	provider    Provider
	log         *log.Logger
	sendTimeout time.Duration

	queue chan Notification
	stop  chan struct{}
	done  chan struct{}

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
		queue:       make(chan Notification, cfg.Buffer),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// Notify enqueues n for asynchronous delivery. Non-blocking: if the
// worker's input queue is full, n is dropped and logged. Safe from any
// goroutine.
//
// We drop rather than block because the caller sits on the host's
// event fan-out path — blocking it would back up event consumption.
// We'd rather lose a notification than starve the rest of the system.
func (l *Loop) Notify(n Notification) {
	select {
	case l.queue <- n:
	default:
		l.log.Printf("queue full; dropping %s notification for session %s", n.Kind, n.SessionID)
	}
}

// Run drives the worker until ctx is canceled or Stop is called.
// Drains the input queue into Provider.Send. Logs (and continues) on
// Send error — retries and DLQs are the Provider's responsibility.
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
		case n := <-l.queue:
			l.send(ctx, n)
		}
	}
}

// drainAndExit empties l.queue under the ambient ctx. send() applies
// its own SendTimeout, so an unresponsive provider doesn't extend
// shutdown indefinitely — it bounds each remaining notification.
func (l *Loop) drainAndExit(ctx context.Context) {
	for {
		select {
		case n := <-l.queue:
			l.send(ctx, n)
		default:
			return
		}
	}
}

func (l *Loop) send(ctx context.Context, n Notification) {
	sendCtx, cancel := context.WithTimeout(ctx, l.sendTimeout)
	defer cancel()
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
