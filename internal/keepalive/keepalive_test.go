package keepalive_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/keepalive"
)

// recordingListener captures every Tick + Close call. A real
// keepalive.Listener impl — not a mock — used as a test fixture so the
// Loop's behavior can be observed without a real provider socket.
type recordingListener struct {
	mu         sync.Mutex
	ticks      []time.Time
	closeCount int
}

func (l *recordingListener) Tick(_ context.Context, lastActivity time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ticks = append(l.ticks, lastActivity)
}

func (l *recordingListener) Close(_ context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeCount++
	return nil
}

func (l *recordingListener) closes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeCount
}

func (l *recordingListener) tickCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ticks)
}

func (l *recordingListener) latestTick() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ticks) == 0 {
		return time.Time{}
	}
	return l.ticks[len(l.ticks)-1]
}

func (l *recordingListener) wasClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeCount > 0
}

// startLoop spins a Loop with sub-second tunables and a Listener,
// returning the Loop and a Stop-on-cleanup helper. Tests should
// call stop() (not l.Stop directly) to ensure the goroutine has
// actually exited before assertions about Close run.
func startLoop(t *testing.T, listener keepalive.Listener, interval, minTick time.Duration) (*keepalive.Loop, func()) {
	t.Helper()
	loop := keepalive.New(keepalive.Config{
		Listener:        listener,
		Interval:        interval,
		MinTickInterval: minTick,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go loop.Run(ctx)
	stop := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = loop.Stop(stopCtx)
		cancel()
	}
	t.Cleanup(stop)
	return loop, stop
}

// TestLoop_TicksAtInterval confirms periodic Ticks fire without any
// Bump activity. Listeners that detect inactivity rely on this.
func TestLoop_TicksAtInterval(t *testing.T) {
	t.Parallel()
	rec := &recordingListener{}
	startLoop(t, rec, 30*time.Millisecond, 1*time.Millisecond)

	deadline := time.After(500 * time.Millisecond)
	for {
		if rec.tickCount() >= 3 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("expected >=3 ticks within 500ms, got %d", rec.tickCount())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestLoop_BumpAdvancesLastActivity confirms that the timestamp on
// each Tick reflects the most recent Bump.
func TestLoop_BumpAdvancesLastActivity(t *testing.T) {
	t.Parallel()
	rec := &recordingListener{}
	loop, _ := startLoop(t, rec, 1*time.Hour, 1*time.Millisecond) // long interval → only Bump-driven ticks

	before := time.Now()
	loop.Bump()
	deadline := time.After(500 * time.Millisecond)
	for {
		if rec.tickCount() >= 1 {
			got := rec.latestTick()
			if !got.After(before.Add(-time.Second)) || got.Before(before) {
				t.Errorf("lastActivity = %v, want between %v and now", got, before)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no Tick after Bump within 500ms")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestLoop_BumpDebouncedByMinTickInterval confirms that a burst of
// Bumps within MinTickInterval produces only one Tick (or close to it),
// not one per Bump.
func TestLoop_BumpDebouncedByMinTickInterval(t *testing.T) {
	t.Parallel()
	rec := &recordingListener{}
	loop, _ := startLoop(t, rec, 1*time.Hour, 200*time.Millisecond)

	// Fire 50 Bumps as fast as possible. With MinTickInterval=200ms,
	// expect exactly 1 Tick within the next ~100ms.
	for i := 0; i < 50; i++ {
		loop.Bump()
	}
	time.Sleep(100 * time.Millisecond)
	if got := rec.tickCount(); got != 1 {
		t.Fatalf("burst of 50 Bumps produced %d ticks, want 1 (debounced)", got)
	}
}

// TestLoop_BumpResumesTicksAfterDebounceWindow confirms that after
// MinTickInterval elapses, a new Bump fires a fresh Tick.
func TestLoop_BumpResumesTicksAfterDebounceWindow(t *testing.T) {
	t.Parallel()
	rec := &recordingListener{}
	loop, _ := startLoop(t, rec, 1*time.Hour, 50*time.Millisecond)

	loop.Bump()
	time.Sleep(20 * time.Millisecond)
	if rec.tickCount() != 1 {
		t.Fatalf("expected 1 tick after first Bump, got %d", rec.tickCount())
	}

	time.Sleep(80 * time.Millisecond) // outside debounce window
	loop.Bump()
	time.Sleep(20 * time.Millisecond)
	if rec.tickCount() != 2 {
		t.Fatalf("expected 2 ticks after second Bump, got %d", rec.tickCount())
	}
}

// TestLoop_ZeroTimeBeforeAnyBump pins that periodic Ticks before any
// Bump carry the zero time.Time so Listeners can distinguish
// "no activity yet" from "activity at the epoch."
func TestLoop_ZeroTimeBeforeAnyBump(t *testing.T) {
	t.Parallel()
	rec := &recordingListener{}
	startLoop(t, rec, 30*time.Millisecond, 1*time.Millisecond)

	deadline := time.After(500 * time.Millisecond)
	for {
		if rec.tickCount() >= 1 {
			if got := rec.latestTick(); !got.IsZero() {
				t.Errorf("first tick lastActivity = %v, want zero", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no tick within 500ms")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestLoop_StopCallsListenerClose pins that Stop drains the goroutine
// and calls Listener.Close exactly once.
func TestLoop_StopCallsListenerClose(t *testing.T) {
	t.Parallel()
	rec := &recordingListener{}
	loop := keepalive.New(keepalive.Config{
		Listener:        rec,
		Interval:        1 * time.Hour,
		MinTickInterval: 1 * time.Millisecond,
	})
	go loop.Run(context.Background())

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := loop.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !rec.wasClosed() {
		t.Error("Listener.Close was not called by Stop")
	}
}

// TestLoop_StopIsIdempotent pins that a second Stop call doesn't
// double-fire Listener.Close. Without the guard, a provider's teardown
// (e.g. DELETE /v1/tasks/agents on Sprites) would run twice on any
// shutdown race.
func TestLoop_StopIsIdempotent(t *testing.T) {
	t.Parallel()
	rec := &recordingListener{}
	loop := keepalive.New(keepalive.Config{
		Listener:        rec,
		Interval:        1 * time.Hour,
		MinTickInterval: 1 * time.Millisecond,
	})
	go loop.Run(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		if err := loop.Stop(ctx); err != nil {
			t.Fatalf("Stop call %d: %v", i, err)
		}
	}
	if got := rec.closes(); got != 1 {
		t.Errorf("Listener.Close called %d times across 3 Stops, want 1", got)
	}
}

// TestLoop_StopIsConcurrencySafe pins that concurrent Stop calls
// don't panic on close-of-closed-channel and don't double-fire
// Listener.Close. Verified under `go test -race`.
func TestLoop_StopIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	rec := &recordingListener{}
	loop := keepalive.New(keepalive.Config{
		Listener:        rec,
		Interval:        1 * time.Hour,
		MinTickInterval: 1 * time.Millisecond,
	})
	go loop.Run(context.Background())

	const stoppers = 16
	var wg sync.WaitGroup
	wg.Add(stoppers)
	for i := 0; i < stoppers; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := loop.Stop(ctx); err != nil {
				t.Errorf("Stop: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := rec.closes(); got != 1 {
		t.Errorf("Listener.Close called %d times across %d concurrent Stops, want 1", got, stoppers)
	}
}

// TestLoop_BumpIsConcurrencySafe pins that many goroutines can Bump
// concurrently with the run loop without races or panics. Verified
// under `go test -race`.
func TestLoop_BumpIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	rec := &recordingListener{}
	loop, _ := startLoop(t, rec, 5*time.Millisecond, 1*time.Millisecond)

	var wg sync.WaitGroup
	const writers = 16
	const bumps = 200
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < bumps; j++ {
				loop.Bump()
			}
		}()
	}
	wg.Wait()
	// The burst can finish before the run goroutine is ever scheduled, so
	// don't assert immediately: a Bump is still pending in the 1-slot
	// channel, guaranteeing a Tick once the loop runs.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(1 * time.Millisecond)
	defer poll.Stop()
	for rec.tickCount() == 0 {
		select {
		case <-deadline.C:
			t.Fatal("no tick within 2s of concurrent Bump burst")
		case <-poll.C:
		}
	}
}

// TestLoop_NewPanicsWithoutListener pins the construction precondition.
func TestLoop_NewPanicsWithoutListener(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("New(Config{Listener:nil}) should panic")
		}
	}()
	_ = keepalive.New(keepalive.Config{})
}
