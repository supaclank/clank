package daemoncli

// Coverage for "MarkRead doesn't stick" — every backend event used to
// re-bump SessionInfo.UpdatedAt unconditionally, so a session would
// flip back to Unread = (UpdatedAt > LastReadAt) = true the moment any
// background event flowed (status pings, ExternalID stamps, etc).
// Now applyEventToMetadata only bumps when something user-visible
// actually changed.

import (
	"context"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
)

// TestMarkRead_StaysReadAfterRedundantEvent replays the production
// scenario: the agent goes idle → user opens session and reads → user
// closes (mark-read fires) → backend emits a no-op event (status
// stays the same, ExternalID re-stamped). Pre-fix, UpdatedAt bumps
// and Unread() returns true again. Post-fix, the dirty check inside
// applyEventToMetadata short-circuits and UpdatedAt stays put.
func TestMarkRead_StaysReadAfterRedundantEvent(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "task")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Tell the host the backend has a real external ID so subsequent
	// "stamp" events look like a no-op merge.
	go b.PushEvent(agent.Event{
		Type:       agent.EventStatusChange,
		ExternalID: "ext-123",
		Timestamp:  time.Now(),
		Data:       agent.StatusChangeData{OldStatus: agent.StatusStarting, NewStatus: agent.StatusIdle},
	})
	// Wait for the status update to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := td.Store.GetSession(ctx, info.ID)
		if got.Status == agent.StatusIdle {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// User reads the session.
	if err := td.Client.Session(info.ID).MarkRead(ctx); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	read, err := td.Store.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if read.Unread() {
		t.Fatalf("session should be read after MarkRead; LastReadAt=%v UpdatedAt=%v", read.LastReadAt, read.UpdatedAt)
	}
	updatedAtBefore := read.UpdatedAt

	// Backend emits a redundant event: same status, same external ID.
	go b.PushEvent(agent.Event{
		Type:       agent.EventStatusChange,
		ExternalID: "ext-123",
		Timestamp:  time.Now(),
		Data:       agent.StatusChangeData{OldStatus: agent.StatusIdle, NewStatus: agent.StatusIdle},
	})
	// Give the relay a chance to process it.
	time.Sleep(150 * time.Millisecond)

	after, err := td.Store.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("Get after redundant event: %v", err)
	}
	if !after.UpdatedAt.Equal(updatedAtBefore) {
		t.Errorf("UpdatedAt bumped on redundant event: before=%v after=%v (regression: marks the session as unread again)", updatedAtBefore, after.UpdatedAt)
	}
	if after.Unread() {
		t.Errorf("session became unread again after a redundant event")
	}
}

// TestMarkRead_StaysReadAfterDuplicateExternalIDStamp pins the
// specific ExternalID-only-stamp case: backends stamp ExternalID onto
// every emit, so per-token part updates carry an ExternalID we already
// have and no user-visible change. Without the dirty check, the
// (info.ExternalID == "") guard is no-op but we still wrote the row
// and bumped UpdatedAt on every token.
func TestMarkRead_StaysReadAfterDuplicateExternalIDStamp(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "task")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First stamp — actually new.
	go b.PushEvent(agent.Event{
		Type:       agent.EventPartUpdate,
		ExternalID: "ext-abc",
		Timestamp:  time.Now(),
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := td.Store.GetSession(ctx, info.ID)
		if got.ExternalID == "ext-abc" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := td.Client.Session(info.ID).MarkRead(ctx); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	read, _ := td.Store.GetSession(ctx, info.ID)
	if read.Unread() {
		t.Fatalf("session should be read after MarkRead")
	}
	updatedAtBefore := read.UpdatedAt

	// Duplicate stamp.
	go b.PushEvent(agent.Event{
		Type:       agent.EventPartUpdate,
		ExternalID: "ext-abc",
		Timestamp:  time.Now(),
	})
	time.Sleep(150 * time.Millisecond)

	after, _ := td.Store.GetSession(ctx, info.ID)
	if !after.UpdatedAt.Equal(updatedAtBefore) {
		t.Errorf("UpdatedAt bumped on duplicate ExternalID stamp: before=%v after=%v", updatedAtBefore, after.UpdatedAt)
	}
	if after.Unread() {
		t.Error("session went back to unread after a duplicate ExternalID stamp")
	}
}

// TestMarkRead_MessageEventMarksUnread pins the flip side: a completed
// message is real activity even when status and ExternalID don't
// change, so it must bump UpdatedAt and mark the session unread —
// recency sorting stays honest for backends that append messages
// without an idle/busy flip.
func TestMarkRead_MessageEventMarksUnread(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "task")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := td.Client.Session(info.ID).MarkRead(ctx); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	read, err := td.Store.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if read.Unread() {
		t.Fatalf("session should be read after MarkRead")
	}
	updatedAtBefore := read.UpdatedAt

	// Unread() compares UpdatedAt strictly after LastReadAt; keep the
	// stamps from landing in the same instant on fast hardware.
	time.Sleep(2 * time.Millisecond)

	go b.PushEvent(agent.Event{
		Type:      agent.EventMessage,
		Timestamp: time.Now(),
		Data:      agent.MessageData{Role: "assistant"},
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := td.Store.GetSession(ctx, info.ID)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if got.UpdatedAt.After(updatedAtBefore) {
			if !got.Unread() {
				t.Error("new message should mark session unread")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("UpdatedAt never bumped after a new message event")
}

// TestMarkRead_DoesNotBumpUpdatedAt is the regression test for the
// "opening a chat hoists the session to the top" bug: MarkRead used
// to bump UpdatedAt on top of LastReadAt, so old sessions would
// appear newest in the inbox just because the user clicked them.
// UpdatedAt is owned by agent activity (status/title from the relay),
// not by user metadata mutations.
func TestMarkRead_DoesNotBumpUpdatedAt(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "task")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	before, err := td.Store.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Wait long enough that a stray time.Now() bump would be detectable.
	time.Sleep(50 * time.Millisecond)

	if err := td.Client.Session(info.ID).MarkRead(ctx); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	after, err := td.Store.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("Get after MarkRead: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("MarkRead bumped UpdatedAt: before=%v after=%v (would hoist the session to top of inbox)", before.UpdatedAt, after.UpdatedAt)
	}
	if after.LastReadAt.IsZero() {
		t.Error("MarkRead did not set LastReadAt")
	}
}

// TestMarkRead_VisibilityAndDraftAndFollowUpDoNotBump pins the same
// invariant for the other user-owned metadata setters. Each of them
// was bumping UpdatedAt via mutateSessionMeta / ToggleSessionFollowUp
// before the fix.
func TestMarkRead_VisibilityAndDraftAndFollowUpDoNotBump(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, _ := td.CreateOpenCodeSession(t, "task")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	before, _ := td.Store.GetSession(ctx, info.ID)

	time.Sleep(20 * time.Millisecond)
	if err := td.Client.Session(info.ID).SetVisibility(ctx, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	got, _ := td.Store.GetSession(ctx, info.ID)
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("SetVisibility bumped UpdatedAt: before=%v after=%v", before.UpdatedAt, got.UpdatedAt)
	}

	time.Sleep(20 * time.Millisecond)
	if err := td.Client.Session(info.ID).SetDraft(ctx, "wip"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}
	got, _ = td.Store.GetSession(ctx, info.ID)
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("SetDraft bumped UpdatedAt: before=%v after=%v", before.UpdatedAt, got.UpdatedAt)
	}

	time.Sleep(20 * time.Millisecond)
	if _, err := td.Client.Session(info.ID).ToggleFollowUp(ctx); err != nil {
		t.Fatalf("ToggleFollowUp: %v", err)
	}
	got, _ = td.Store.GetSession(ctx, info.ID)
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("ToggleFollowUp bumped UpdatedAt: before=%v after=%v", before.UpdatedAt, got.UpdatedAt)
	}
}

// TestMarkRead_BumpsOnRealStatusChange confirms we still detect a
// genuine state change. A regression that made every event a no-op
// would silently break "session went idle" and "title arrived"
// notifications.
func TestMarkRead_BumpsOnRealStatusChange(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "task")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := td.Client.Session(info.ID).MarkRead(ctx); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	before, _ := td.Store.GetSession(ctx, info.ID)
	if before.Unread() {
		t.Fatalf("precondition: should be read")
	}

	// Ensure the event's UpdatedAt stamp lands strictly after MarkRead's
	// LastReadAt stamp. Unread() uses UpdatedAt.After(LastReadAt), and
	// without this gap both time.Now() calls can fall in the same
	// nanosecond on fast hardware, making the assertion flaky.
	time.Sleep(2 * time.Millisecond)

	// Real status change (idle → busy). Should bump UpdatedAt → unread.
	go b.PushEvent(agent.Event{
		Type:      agent.EventStatusChange,
		Timestamp: time.Now(),
		Data:      agent.StatusChangeData{OldStatus: agent.StatusIdle, NewStatus: agent.StatusBusy},
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := td.Store.GetSession(ctx, info.ID)
		if got.Status == agent.StatusBusy {
			if !got.Unread() {
				t.Error("real status change should mark session unread")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("status never converged to busy")
}
