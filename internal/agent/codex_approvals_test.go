package agent

import (
	"context"
	"testing"
	"time"
)

// approvalResult carries parkPermission's return across the test goroutine.
type approvalResult struct {
	allow bool
}

func TestCodexPermissionRoundTrip(t *testing.T) {
	t.Parallel()
	b := newTestCodexBackend()

	done := make(chan approvalResult, 1)
	go func() {
		allow := b.parkPermission(context.Background(), codexToolShell, "shell: rm -rf /tmp/x", "exec-42")
		done <- approvalResult{allow: allow}
	}()

	// The park emits the permission event; grab its request id.
	var perm PermissionData
	select {
	case ev := <-b.events:
		perm = ev.Data.(PermissionData)
	case <-time.After(5 * time.Second):
		t.Fatal("no permission event emitted")
	}
	if perm.Tool != codexToolShell || perm.ToolUseID != "exec-42" {
		t.Errorf("permission = %+v", perm)
	}

	if err := b.RespondPermission(context.Background(), perm.RequestID, true, ""); err != nil {
		t.Fatalf("respond: %v", err)
	}
	select {
	case res := <-done:
		if !res.allow {
			t.Error("park returned deny, want allow")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("park never returned after respond")
	}

	// The id is single-use: a second response fails fast.
	if err := b.RespondPermission(context.Background(), perm.RequestID, true, ""); err == nil {
		t.Error("second respond on same id succeeded, want error")
	}
}

func TestCodexAbortFailsPendingPermissions(t *testing.T) {
	t.Parallel()
	b := newTestCodexBackend()

	done := make(chan approvalResult, 1)
	go func() {
		allow := b.parkPermission(context.Background(), codexToolShell, "shell: sleep 100", "exec-7")
		done <- approvalResult{allow: allow}
	}()

	select {
	case <-b.events: // permission event
	case <-time.After(5 * time.Second):
		t.Fatal("no permission event emitted")
	}

	// Abort with no live client: must still free the parked approval.
	if err := b.Abort(context.Background()); err != nil {
		t.Fatalf("abort: %v", err)
	}
	select {
	case res := <-done:
		if res.allow {
			t.Error("aborted park returned allow, want deny")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("park never returned after abort")
	}
}

func TestCodexRespondPermissionUnknownID(t *testing.T) {
	t.Parallel()
	b := newTestCodexBackend()
	if err := b.RespondPermission(context.Background(), "perm-999", true, ""); err == nil {
		t.Error("unknown permission id accepted, want error")
	}
}

func TestCodexRevertAndQuestionsUnsupported(t *testing.T) {
	t.Parallel()
	b := newTestCodexBackend()
	if err := b.Revert(context.Background(), "some-turn"); err == nil {
		t.Error("Revert succeeded, want unsupported error")
	}
	if err := b.RespondQuestion(context.Background(), "q-1", nil, false); err == nil {
		t.Error("RespondQuestion succeeded, want unsupported error")
	}
}
