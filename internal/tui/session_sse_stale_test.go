package tui

import (
	"testing"

	"github.com/supaclank/clank/internal/agent"
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

	cmd := waitForEvent(ch)
	msg := cmd()

	closed, ok := msg.(sseClosedMsg)
	if !ok {
		t.Fatalf("closed channel produced %T, want sseClosedMsg", msg)
	}
	if closed.events != ch {
		t.Fatal("sseClosedMsg does not carry the closed channel; Update cannot tell stale closures from current ones")
	}
}

// TestSessionView_StaleSourceEventDoesNotReArmLiveChannel guards the
// duplicate-goroutine leak: when a sessionEventMsg arrives from a *stale*
// subscription (e.g. a since-cancelled chA delivered a buffered event
// before closing), Update must not re-arm waitForEvent on m.eventsCh
// (the live chB). Doing so would spawn an extra waiter every time a
// stale event landed, leaking goroutines and racing the legitimate
// waiter for chB events.
//
// The original sessionEventMsg carried only the event payload, so Update
// had no way to tell which channel produced it. Tagging the message with
// its source channel lets Update gate the re-arm on identity.
func TestSessionView_StaleSourceEventDoesNotReArmLiveChannel(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "session-B")
	liveCh := make(chan agent.Event, 1)
	m.eventsCh = liveCh
	staleCh := make(chan agent.Event, 1)

	stale := sessionEventMsg{
		sourceCh: staleCh,
		event:    agent.Event{Type: agent.EventPartUpdate, SessionID: "session-A"},
	}
	_, cmd := m.Update(stale)
	if cmd != nil {
		t.Fatal("stale-source event re-armed waitForEvent on the live channel; that would spawn a duplicate goroutine and leak each time a stale event landed")
	}
}

// TestSessionView_LiveSourceEventReArmsLiveChannel is the positive
// counterpart: when sessionEventMsg DOES come from m.eventsCh, Update
// must re-arm so the wait loop keeps consuming events.
func TestSessionView_LiveSourceEventReArmsLiveChannel(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "session-B")
	liveCh := make(chan agent.Event, 1)
	m.eventsCh = liveCh

	// Even a filtered-out event (different session) from the live channel
	// must re-arm — the live waiter has already returned, so without a
	// new arm the wait loop dies.
	filteredButLive := sessionEventMsg{
		sourceCh: liveCh,
		event:    agent.Event{Type: agent.EventPartUpdate, SessionID: "session-A"},
	}
	_, cmd := m.Update(filteredButLive)
	if cmd == nil {
		t.Fatal("live-source event must re-arm waitForEvent or the wait loop dies after the first filtered event")
	}
}

// TestWaitForEvent_DoesNotSwallowEventsForOtherSessions guards the
// stale-waiter-eats-live-event hole: waitForEvent must not filter events
// by sessionID before Update sees them. A waiter scheduled before a view
// swap closes over a since-stale sessionID; if it consumes a live event
// for the new view and emits a "skip" signal, the event is gone — the
// new view's own waiter never sees it. The only place with the current
// m.sessionID is Update, so filtering belongs there.
func TestWaitForEvent_DoesNotSwallowEventsForOtherSessions(t *testing.T) {
	t.Parallel()

	ch := make(chan agent.Event, 1)
	ch <- agent.Event{Type: agent.EventPartUpdate, SessionID: "session-B"}

	cmd := waitForEvent(ch)
	msg := cmd()

	evtMsg, ok := msg.(sessionEventMsg)
	if !ok {
		t.Fatalf("waitForEvent dropped a live event into %T; the event is lost to the live view", msg)
	}
	if evtMsg.event.SessionID != "session-B" {
		t.Fatalf("event SessionID = %q, want %q", evtMsg.event.SessionID, "session-B")
	}
}
