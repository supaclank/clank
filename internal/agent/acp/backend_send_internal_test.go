package acp

import (
	"context"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
)

// drainLateUpdates must not ride out the quiet window once bgCtx is
// cancelled — Stop() cancels bgCtx without waiting for runTurns, so a
// stuck drain leaks the goroutine past test/backend teardown (observed as
// late logf calls after the caller has moved on).
func TestDrainLateUpdates_ExitsOnContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	b := &Backend{bgCtx: ctx}
	b.lastUpdate.set(time.Now()) // fresh, so the window starts under drainQuiet

	start := time.Now()
	b.drainLateUpdates(time.Now())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("drainLateUpdates took %v after ctx cancellation, want well under drainQuiet (%v)", elapsed, drainQuiet)
	}
}

// The quiet window must be floored on the turn's own start: lastUpdate is
// session-scoped, so a turn whose updates haven't been processed yet sees
// a stale (or zero) value. Unfloored, that exits the drain instantly — the
// CI-only failure where the first turn of a session committed before its
// tool_call arrived, leaking the tool's updates into a phantom next turn.
func TestDrainLateUpdates_FloorsWindowOnTurnStart(t *testing.T) {
	t.Parallel()
	b := &Backend{bgCtx: context.Background()}
	// Zero lastUpdate: an unfloored time.Since() reads as decades of quiet.

	start := time.Now()
	b.drainLateUpdates(time.Now())
	if elapsed := time.Since(start); elapsed < drainQuiet {
		t.Errorf("drain returned after %v with a zero lastUpdate, want >= drainQuiet (%v)", elapsed, drainQuiet)
	}
}

// watchConn sets StatusDead as soon as the transport dies, racing
// runTurns' own handling of the RPC error that same death produces.
// Once Dead has already landed, runTurns must fold that error into the
// swallow path instead of regressing status to Error.
func TestShouldSwallowPromptErrLocked_TransportAlreadyDead(t *testing.T) {
	t.Parallel()
	b := &Backend{status: agent.StatusDead}
	if !b.shouldSwallowPromptErrLocked(false) {
		t.Error("a Prompt error after watchConn already marked the session dead must be swallowed, not regress to Error")
	}
}

func TestShouldSwallowPromptErrLocked_AbortingOrStopping(t *testing.T) {
	t.Parallel()
	if b := (&Backend{status: agent.StatusBusy}); !b.shouldSwallowPromptErrLocked(true) {
		t.Error("an aborting turn's RPC error must be swallowed")
	}
	if b := (&Backend{status: agent.StatusBusy, stopping: true}); !b.shouldSwallowPromptErrLocked(false) {
		t.Error("a stopping backend's RPC error must be swallowed")
	}
}

func TestShouldSwallowPromptErrLocked_GenuineError(t *testing.T) {
	t.Parallel()
	b := &Backend{status: agent.StatusBusy}
	if b.shouldSwallowPromptErrLocked(false) {
		t.Error("a live, non-aborting turn's RPC error must still surface as StatusError")
	}
}

// Send's second lock (after attachment resolution / mode / model calls)
// re-checks only b.stopping, not b.status — so a watchConn-triggered Dead
// landing between the two locks got silently overwritten with Busy,
// stranding the host's rebuild contract (which is keyed on StatusDead).
func TestBackend_Send_RejectsWhenTransportDead(t *testing.T) {
	t.Parallel()
	b := NewBackend(AdapterProfile{}, "/work", "", "", nil, nil, nil)
	b.opened = true
	b.conn = &AdapterConn{}
	b.status = agent.StatusDead

	if err := b.Send(context.Background(), agent.SendMessageOpts{Text: "hi"}); err == nil {
		t.Error("Send on a dead backend must error, not resurrect status to Busy")
	}
	if b.status != agent.StatusDead {
		t.Errorf("status = %v after Send, want StatusDead preserved", b.status)
	}
	if b.runnerOn {
		t.Error("Send must not start a turn runner against a dead connection")
	}
}
