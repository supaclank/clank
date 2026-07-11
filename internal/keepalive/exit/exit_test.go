package exit

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock shared by the Listener and
// the test.
type fakeClock struct{ now atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.now.Store(time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.now.Add(int64(d)) }

func newTestListener(t *testing.T, clock *fakeClock) (*Listener, *atomic.Int64) {
	t.Helper()
	var fires atomic.Int64
	l := New(Options{
		Shutdown:      func() { fires.Add(1) },
		IdleThreshold: 3 * time.Minute,
		Log:           log.New(io.Discard, "", 0),
		Now:           clock.Now,
	})
	return l, &fires
}

func TestNoFireBeforeWarmupGrace(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, fires := newTestListener(t, clock)

	// A machine woken by the edge for a single request Ticks with zero
	// lastActivity — process start must count as activity.
	l.Tick(context.Background(), time.Time{})
	clock.Advance(2 * time.Minute)
	l.Tick(context.Background(), time.Time{})
	if fires.Load() != 0 {
		t.Fatal("fired during warm-up grace")
	}
}

func TestFiresOnceAfterIdleThreshold(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, fires := newTestListener(t, clock)

	clock.Advance(3*time.Minute + time.Second)
	l.Tick(context.Background(), time.Time{})
	if fires.Load() != 1 {
		t.Fatalf("want exactly one fire, got %d", fires.Load())
	}
	// Subsequent ticks while shutdown is in flight must not re-fire.
	clock.Advance(time.Minute)
	l.Tick(context.Background(), time.Time{})
	if fires.Load() != 1 {
		t.Fatalf("re-fired after shutdown initiated: %d", fires.Load())
	}
}

func TestRecentBackendActivityVetoes(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, fires := newTestListener(t, clock)

	clock.Advance(10 * time.Minute)
	recentEvent := clock.Now().Add(-time.Minute)
	l.Tick(context.Background(), recentEvent)
	if fires.Load() != 0 {
		t.Fatal("fired despite backend event 1m ago")
	}

	clock.Advance(3 * time.Minute)
	l.Tick(context.Background(), recentEvent)
	if fires.Load() != 1 {
		t.Fatal("did not fire once the backend event aged out")
	}
}

func TestRecentRequestVetoes(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, fires := newTestListener(t, clock)

	h := l.TrackHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	clock.Advance(10 * time.Minute)
	// A user browsing repos produces requests but no backend events.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos", nil))

	clock.Advance(2 * time.Minute)
	l.Tick(context.Background(), time.Time{})
	if fires.Load() != 0 {
		t.Fatal("fired despite an HTTP request 2m ago")
	}

	clock.Advance(2 * time.Minute)
	l.Tick(context.Background(), time.Time{})
	if fires.Load() != 1 {
		t.Fatal("did not fire once request activity aged out")
	}
}

func TestInFlightRequestVetoesRegardlessOfAge(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, fires := newTestListener(t, clock)

	release := make(chan struct{})
	served := make(chan struct{})
	h := l.TrackHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(served)
		<-release // an SSE stream / tunnel bridge parked here for hours
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/events", nil))
	}()
	<-served

	clock.Advance(time.Hour)
	l.Tick(context.Background(), time.Time{})
	if fires.Load() != 0 {
		t.Fatal("fired while a request was in flight")
	}

	close(release)
	<-done

	// The idle clock restarts when the long request ENDS.
	l.Tick(context.Background(), time.Time{})
	if fires.Load() != 0 {
		t.Fatal("fired immediately after the in-flight request ended")
	}
	clock.Advance(3*time.Minute + time.Second)
	l.Tick(context.Background(), time.Time{})
	if fires.Load() != 1 {
		t.Fatal("did not fire after the post-request idle window")
	}
}
