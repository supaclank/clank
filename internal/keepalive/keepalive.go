// Package keepalive sends an "agent is active" signal to a provider-
// specific Listener whenever a backend event arrives. Each Tick carries
// the latest activity timestamp; the Listener decides what to do with
// it (renew a sandbox lease, write a TTL record, trigger shutdown after
// inactivity, …).
//
// The package name describes the *active* mechanism — sending ticks
// while there's work — not the *goal*, which is the opposite: let the
// sandbox hibernate as soon as work is done. Sprites' problem is that
// it hibernates too eagerly; keepalive fights that. Other providers may
// have the opposite problem (never auto-shut), and a Listener for those
// would Tick into a shutdown decision instead. The Loop is agnostic.
//
// Backpressure: callers (the host's subscriber loop) feed activity via
// Bump, which is a non-blocking send to a 1-slot channel. Bursts coalesce.
// Listeners therefore see at most one Tick per Bump, plus periodic ones
// at Interval. Any single surviving event refreshes lastActivity — a
// dropped event is harmless as long as one within the burst gets through.
package keepalive

import (
	"context"
	"log"
	"sync"
	"time"
)

// Listener is called by the Loop with the latest known activity
// timestamp on each tick. Each implementation decides what to do:
//   - Sprites/Modal-style (renew lease):  PUT while time.Since(lastActivity) < threshold.
//   - Inactivity-detection (we kill):     trigger shutdown when time.Since(lastActivity) > threshold.
//   - Webhook forwarder:                  POST the timestamp; consumer decides.
//   - TTL outbox:                         upsert a TTL record keyed on lastActivity.
//
// Tick may fire with the zero time.Time before any Bump has happened.
// Implementations should treat zero as "no activity yet".
//
// Tick should be cheap and non-blocking. It runs in the Loop's
// goroutine; a slow Tick blocks both the ticker and subsequent Bumps.
type Listener interface {
	Tick(ctx context.Context, lastActivity time.Time)
	Close(ctx context.Context) error
}

const (
	// DefaultInterval is the periodic Tick cadence. The Loop calls
	// Listener.Tick every Interval regardless of activity, so Listeners
	// that detect inactivity (compare time.Since(lastActivity) > X) get
	// a chance to act even without Bumps.
	DefaultInterval = 30 * time.Second

	// DefaultMinTickInterval is the debounce floor between consecutive
	// Ticks. A burst of Bumps within this window produces a single Tick.
	DefaultMinTickInterval = 5 * time.Second
)

// Config is the Loop's construction config. Listener is required; the
// rest defaults to Default* when zero. Construct via New.
type Config struct {
	Listener        Listener
	Interval        time.Duration
	MinTickInterval time.Duration
	Log             *log.Logger
}

// Loop ticks the Listener on Interval and on Bump (debounced). Construct
// with New, drive activity via Bump, run with Run, stop with Stop.
type Loop struct {
	listener        Listener
	interval        time.Duration
	minTickInterval time.Duration
	log             *log.Logger

	bumps chan struct{}
	stop  chan struct{}
	done  chan struct{}

	// stopOnce guards close(l.stop); closeOnce guards Listener.Close.
	// Both protect against duplicate or concurrent Stop calls — a
	// second close(l.stop) would panic, and a second Listener.Close
	// would double-fire provider teardown (e.g. two DELETEs to the
	// Sprites Tasks API).
	stopOnce  sync.Once
	closeOnce sync.Once
	closeErr  error

	mu           sync.Mutex
	lastActivity time.Time
	lastTick     time.Time
}

// New constructs a Loop. Panics on missing Listener — fast failure beats
// a later nil deref. Run must be called to start the loop.
func New(cfg Config) *Loop {
	if cfg.Listener == nil {
		panic("keepalive.New: Listener is required")
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MinTickInterval == 0 {
		cfg.MinTickInterval = DefaultMinTickInterval
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	return &Loop{
		listener:        cfg.Listener,
		interval:        cfg.Interval,
		minTickInterval: cfg.MinTickInterval,
		log:             cfg.Log,
		bumps:           make(chan struct{}, 1),
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
}

// Bump signals that activity happened. Safe from any goroutine and
// non-blocking — if a Bump is already pending the new one coalesces
// into it. Callers in hot paths (the host's subscriber loop) call
// this for every event.
func (l *Loop) Bump() {
	select {
	case l.bumps <- struct{}{}:
	default:
	}
}

// Run drives the Loop until ctx is canceled or Stop is called. Ticks
// the Listener on l.interval and on Bump (debounced to at most one
// per l.minTickInterval).
func (l *Loop) Run(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stop:
			return
		case <-l.bumps:
			l.recordActivity(time.Now())
			l.maybeTick(ctx)
		case <-ticker.C:
			l.maybeTick(ctx)
		}
	}
}

// Stop halts Run and calls Listener.Close. Safe to call multiple times
// concurrently: close(l.stop) and Listener.Close each fire exactly once;
// subsequent calls return the cached error from the first Close. If
// ctx fires before Run exits, Stop returns ctx.Err() without invoking
// Listener.Close — a subsequent Stop with a live ctx will still close.
func (l *Loop) Stop(ctx context.Context) error {
	l.stopOnce.Do(func() { close(l.stop) })
	select {
	case <-l.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	l.closeOnce.Do(func() {
		l.closeErr = l.listener.Close(ctx)
	})
	return l.closeErr
}

func (l *Loop) recordActivity(t time.Time) {
	l.mu.Lock()
	l.lastActivity = t
	l.mu.Unlock()
}

func (l *Loop) maybeTick(ctx context.Context) {
	l.mu.Lock()
	now := time.Now()
	if !l.lastTick.IsZero() && now.Sub(l.lastTick) < l.minTickInterval {
		l.mu.Unlock()
		return
	}
	l.lastTick = now
	last := l.lastActivity
	l.mu.Unlock()
	l.listener.Tick(ctx, last)
}
