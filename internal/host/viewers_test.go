package host

import (
	"context"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/notifier"
)

func TestSessionViewers_CountAndRelease(t *testing.T) {
	t.Parallel()
	svc := New(Options{BackendManagers: map[agent.BackendType]agent.BackendManager{}})
	t.Cleanup(svc.Shutdown)

	if svc.SessionHasViewers("s1") {
		t.Fatal("fresh service must have no viewers")
	}
	rel1 := svc.AddSessionViewer("s1")
	rel2 := svc.AddSessionViewer("s1")
	if !svc.SessionHasViewers("s1") {
		t.Error("expected viewers after registration")
	}
	if svc.SessionHasViewers("s2") {
		t.Error("presence must be per-session")
	}
	rel1()
	if !svc.SessionHasViewers("s1") {
		t.Error("one of two viewers released — session must still count as viewed")
	}
	rel2()
	if svc.SessionHasViewers("s1") {
		t.Error("all viewers released — session must not count as viewed")
	}
}

// TestNotifier_ViewedSessionSuppressesIdlePush documents the guest-app
// overlay scenario: the user chats via the floating prompt box, whose
// event stream registers as a viewer — the finish is already on their
// screen, so pushing "Agent finished" ~1s later is noise. Permission
// pushes still go out while viewed: missing one because a stale viewer
// socket lingered would leave the agent blocked.
func TestNotifier_ViewedSessionSuppressesIdlePush(t *testing.T) {
	t.Parallel()
	svc, rec := newTestServiceWithNotifier(t)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	release := svc.AddSessionViewer("s1")

	idle := agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "s1",
		Data:      agent.StatusChangeData{OldStatus: agent.StatusBusy, NewStatus: agent.StatusIdle},
	}
	svc.subscribers.Broadcast(idle)
	// Permission doubles as the serialization point: when it arrives at
	// the provider, the fan-in has already examined (and dropped) the
	// idle event before it.
	svc.subscribers.Broadcast(agent.Event{
		Type:      agent.EventPermission,
		SessionID: "s1",
		Data:      agent.PermissionData{RequestID: "r1", Tool: "bash"},
	})

	if got := waitForNotifierSent(rec, 1, 500*time.Millisecond); got != 1 {
		t.Fatalf("got %d notifications, want 1 (permission only; idle suppressed)", got)
	}
	if got := rec.sentSnapshot()[0].Kind; got != notifier.KindPermission {
		t.Errorf("Kind = %q, want %q (idle must be the suppressed one)", got, notifier.KindPermission)
	}

	// Viewer gone → idle pushes flow again.
	release()
	svc.subscribers.Broadcast(idle)
	if got := waitForNotifierSent(rec, 2, 500*time.Millisecond); got != 2 {
		t.Fatalf("got %d notifications, want 2 (idle must push once unviewed)", got)
	}
	if got := rec.sentSnapshot()[1].Kind; got != notifier.KindIdle {
		t.Errorf("Kind = %q, want %q", got, notifier.KindIdle)
	}
}
