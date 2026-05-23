package tui

import (
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// These regression tests cover the stuck-streaming-after-switch bug:
//
// When the user switches session A → B, replaceSessionView cancels A's SSE
// subscription. A's HTTP context cancellation races with TCP teardown — in
// the unlucky branch parseSSEStream observes io.EOF on its body Read instead
// of context.Canceled. That path produces no synthetic EventError before the
// deferred close(ch), so A's still-in-flight waitForEvent reads ok=false and
// emits sseClosedMsg. The message lands at Update on the *new* SessionViewModel
// (B), and the previous handler unconditionally nil'd m.eventsCh — but
// m.eventsCh was now B's still-live channel. After that, sessionEventMsg's
// `if m.eventsCh != nil` gate stops re-scheduling waitForEvent, the channel
// fills up, the daemon's Broadcast hits its default-drop branch, and the UI
// freezes mid-stream. The user's symptom: idle→busy event renders, then no
// more deltas; re-entering shows a partial assistant message with a spinner
// that never resolves.
//
// Fix: sseClosedMsg now carries the closed channel; the handler only nil's
// m.eventsCh when the closed channel IS the current one.

// TestSessionView_StaleSseClosedDoesNotNilCurrentChannel guards the fix:
// a sseClosedMsg from a *previous* subscription must not nil the current
// view's eventsCh.
func TestSessionView_StaleSseClosedDoesNotNilCurrentChannel(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "session-B")
	currentCh := make(chan agent.Event, 1)
	m.eventsCh = currentCh

	// A's already-cancelled, closed channel (parseSSEStream's defer close).
	staleCh := make(chan agent.Event)
	close(staleCh)

	_, _ = m.Update(sseClosedMsg{events: staleCh})

	if m.eventsCh != currentCh {
		t.Fatal("stale sseClosedMsg from a previous subscription nil'd the current eventsCh; the UI would freeze mid-stream")
	}
}

// TestSessionView_OwnSseClosedNilsChannel guards against over-correction:
// when the *current* channel closes, m.eventsCh must still be nil'd so the
// view stops trying to re-schedule waitForEvent on a dead channel.
func TestSessionView_OwnSseClosedNilsChannel(t *testing.T) {
	t.Parallel()

	m := NewSessionViewModel(nil, "session-B")
	ch := make(chan agent.Event, 1)
	m.eventsCh = ch

	_, _ = m.Update(sseClosedMsg{events: ch})

	if m.eventsCh != nil {
		t.Fatal("close of current eventsCh must nil m.eventsCh; otherwise the re-schedule chain spins forever on a dead channel")
	}
}

// TestWaitForEvent_ClosedChannelCarriesIdentity verifies that the message
// emitted when the channel closes contains the *closed* channel — without
// that, the handler can't distinguish stale closures from the current one.
// This is the actual wire-up between waitForEvent and the stale-closure
// fix.
func TestWaitForEvent_ClosedChannelCarriesIdentity(t *testing.T) {
	t.Parallel()

	ch := make(chan agent.Event)
	close(ch)

	cmd := waitForEvent(ch, "session-X")
	msg := cmd()

	closed, ok := msg.(sseClosedMsg)
	if !ok {
		t.Fatalf("closed channel produced %T, want sseClosedMsg", msg)
	}
	if closed.events != ch {
		t.Fatal("sseClosedMsg does not carry the closed channel; Update cannot tell stale closures from current ones")
	}
}
