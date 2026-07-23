package acp

import (
	"context"
	"testing"
	"time"
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
