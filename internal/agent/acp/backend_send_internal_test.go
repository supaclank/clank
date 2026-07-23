package acp

import (
	"context"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
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
	b.lastUpdate.set(time.Now()) // fresh, so sinceSet() starts under drainQuiet

	start := time.Now()
	b.drainLateUpdates()
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("drainLateUpdates took %v after ctx cancellation, want well under drainQuiet (%v)", elapsed, drainQuiet)
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
