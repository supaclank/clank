// Package exit implements a keepalive.Listener that shuts the process
// down after a period of inactivity — the "inactivity-detection (we
// kill)" listener anticipated by the keepalive package doc.
//
// It inverts the sprites lease model for providers where compute
// stops when the main process exits (Fly Machines with restart policy
// "no"): instead of renewing a lease while active, clank-host exits
// cleanly once idle and the provider's edge restarts it on the next
// request.
//
// "Active" is the union of four signals:
//   - backend events (the Tick's lastActivity, fed by agent sessions)
//   - HTTP traffic (every request through TrackHTTP refreshes the clock)
//   - in-flight requests (open SSE streams, tunnel bridges — any
//     request still being served vetoes exit regardless of timestamps)
//   - busy sessions (Options.Busy > 0 vetoes exit AND counts as
//     activity): a single long tool execution emits NO backend events
//     between tool start and result, so event-fed activity can go
//     silent for many minutes while an agent is mid-turn. Without this
//     veto the host self-exits under a running agent, killing the CLI
//     and truncating the turn (observed 2026-07-15: a 3m10s "idle"
//     shutdown mid-scaffold after the phone app closed its stream).
//
// Process start counts as activity so a machine woken for a single
// request always gets a full idle window to serve it.
package exit

import (
	"context"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// DefaultIdleThreshold matches the sprites listener's window: the
// host shuts down ~3 minutes after the last sign of life.
const DefaultIdleThreshold = 3 * time.Minute

// Options configures the Listener. Shutdown is required — it must
// initiate a graceful process shutdown (production sends SIGTERM to
// self so the server drains through the normal signal path).
type Options struct {
	Shutdown      func()
	IdleThreshold time.Duration // zero → DefaultIdleThreshold
	Log           *log.Logger   // nil → log.Default()

	// Busy reports the number of agent sessions currently mid-turn
	// (status busy/starting). While > 0 the listener never exits and
	// stamps activity, so the idle window restarts from the moment the
	// last turn ends. Nil means no busy source (tests, callers without
	// session tracking).
	Busy func() int

	// Now overrides the clock. Tests inject a controllable clock to
	// drive the decision table without sleeping. Nil means time.Now.
	Now func() time.Time
}

// Listener triggers Options.Shutdown when idle. Construct via New,
// wrap the host's HTTP handler with TrackHTTP, and register with the
// keepalive Loop like any other Listener.
type Listener struct {
	shutdown      func()
	idleThreshold time.Duration
	log           *log.Logger
	busy          func() int
	now           func() time.Time

	startedAt   time.Time
	inflight    atomic.Int64
	lastRequest atomic.Int64 // unix nanos of the most recent request
	fired       atomic.Bool
}

// New constructs a Listener. Panics on a missing Shutdown — a nil
// trigger would silently disable the provider's only stop mechanism.
func New(opts Options) *Listener {
	if opts.Shutdown == nil {
		panic("exit.New: Shutdown is required")
	}
	if opts.IdleThreshold == 0 {
		opts.IdleThreshold = DefaultIdleThreshold
	}
	if opts.Log == nil {
		opts.Log = log.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Listener{
		shutdown:      opts.Shutdown,
		idleThreshold: opts.IdleThreshold,
		log:           opts.Log,
		busy:          opts.Busy,
		now:           opts.Now,
		startedAt:     opts.Now(),
	}
}

// TrackHTTP counts in-flight requests and stamps request activity.
// Long-lived requests (SSE, tunnel bridges) hold the in-flight count
// for their whole duration, vetoing exit while a client is attached.
func (l *Listener) TrackHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.inflight.Add(1)
		defer func() {
			// Stamp on completion too: the idle clock starts when a
			// long request ENDS, not when it began.
			l.lastRequest.Store(l.now().UnixNano())
			l.inflight.Add(-1)
		}()
		l.lastRequest.Store(l.now().UnixNano())
		next.ServeHTTP(w, r)
	})
}

// Tick implements keepalive.Listener: fire Shutdown exactly once when
// every activity signal is older than the idle threshold and nothing
// is in flight.
func (l *Listener) Tick(_ context.Context, lastActivity time.Time) {
	if l.fired.Load() {
		return
	}
	if l.inflight.Load() > 0 {
		return
	}
	if l.busy != nil && l.busy() > 0 {
		// Mid-turn: veto AND stamp, so the idle window restarts from
		// the moment the last turn ends even if the turn's final events
		// are sparse.
		l.lastRequest.Store(l.now().UnixNano())
		return
	}
	last := l.startedAt
	if lastActivity.After(last) {
		last = lastActivity
	}
	if lr := time.Unix(0, l.lastRequest.Load()); lr.After(last) {
		last = lr
	}
	idle := l.now().Sub(last)
	if idle < l.idleThreshold {
		return
	}
	if l.inflight.Load() > 0 {
		return
	}
	if l.fired.CompareAndSwap(false, true) {
		l.log.Printf("exit keepalive: idle %s (threshold %s), initiating shutdown", idle.Round(time.Second), l.idleThreshold)
		l.shutdown()
	}
}

// Close implements keepalive.Listener. Nothing to release — the
// process is already on its way down when this runs.
func (l *Listener) Close(context.Context) error { return nil }
