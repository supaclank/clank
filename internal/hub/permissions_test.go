package hub_test

import (
	"context"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/hub"
)

func TestDaemonPermissionReply(t *testing.T) {
	mgr := newMockBackendManager()

	s := hub.New()
	s.BackendManagers[agent.BackendOpenCode] = mgr
	s.BackendManagers[agent.BackendClaudeCode] = mgr

	client, _, cleanup := startHubOnSocket(t, s)
	defer cleanup()
	registerTestRepo(t, s)

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "do stuff",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(0) // keep import in scope; replaced by waitFor below
	waitFor(t, 2*time.Second, "backend started", func() bool {
		return mgr.getLatest() != nil
	})

	err = client.Session(info.ID).ReplyPermission(ctx, "perm-42", true)
	if err != nil {
		t.Fatalf("ReplyPermission: %v", err)
	}

	b := mgr.getLatest()
	waitFor(t, 2*time.Second, "permission reply propagated to backend", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.permissionReplied
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.permissionReplied {
		t.Error("expected backend to have received permission reply")
	}
	if b.permissionID != "perm-42" {
		t.Errorf("permissionID = %q, want %q", b.permissionID, "perm-42")
	}
	if !b.permissionAllow {
		t.Error("expected permissionAllow = true")
	}
}

func TestDaemonPermissionReplyNotFound(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	err := client.Session("nonexistent").ReplyPermission(context.Background(), "perm-1", true)
	if err == nil {
		t.Error("expected error replying to permission on non-existent session")
	}
}

func TestDaemonPendingPermission(t *testing.T) {
	t.Parallel()

	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "do stuff",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitFor(t, 2*time.Second, "backend started", func() bool {
		return getBackend() != nil
	})

	// No pending permission yet.
	perms, err := client.Session(info.ID).PendingPermissions(ctx)
	if err != nil {
		t.Fatalf("GetPendingPermissions: %v", err)
	}
	if len(perms) != 0 {
		t.Fatalf("expected no pending permissions, got %d", len(perms))
	}

	// Simulate the backend emitting a permission event.
	b := getBackend()
	b.events <- agent.Event{
		Type:      agent.EventPermission,
		Timestamp: time.Now(),
		Data: agent.PermissionData{
			RequestID:   "perm-99",
			Tool:        "bash",
			Description: "rm -rf /",
		},
	}

	// Wait for the event relay goroutine to surface the pending perm.
	waitFor(t, 2*time.Second, "pending permission visible to client", func() bool {
		got, err := client.Session(info.ID).PendingPermissions(ctx)
		return err == nil && len(got) == 1
	})

	// Now pending permission should be available.
	perms, err = client.Session(info.ID).PendingPermissions(ctx)
	if err != nil {
		t.Fatalf("GetPendingPermissions after emit: %v", err)
	}
	if len(perms) != 1 {
		t.Fatalf("expected 1 pending permission, got %d", len(perms))
	}
	if perms[0].RequestID != "perm-99" {
		t.Errorf("RequestID = %q, want %q", perms[0].RequestID, "perm-99")
	}
	if perms[0].Tool != "bash" {
		t.Errorf("Tool = %q, want %q", perms[0].Tool, "bash")
	}
	if perms[0].Description != "rm -rf /" {
		t.Errorf("Description = %q, want %q", perms[0].Description, "rm -rf /")
	}

	// Reply to the permission — should clear the pending state.
	if err := client.Session(info.ID).ReplyPermission(ctx, "perm-99", true); err != nil {
		t.Fatalf("ReplyPermission: %v", err)
	}

	perms, err = client.Session(info.ID).PendingPermissions(ctx)
	if err != nil {
		t.Fatalf("GetPendingPermissions after reply: %v", err)
	}
	if len(perms) != 0 {
		t.Fatalf("expected pending permissions cleared after reply, got %d", len(perms))
	}
}

func TestDaemonPendingPermissionNotFound(t *testing.T) {
	t.Parallel()

	_, client, cleanup := testDaemon(t)
	defer cleanup()

	_, err := client.Session("nonexistent").PendingPermissions(context.Background())
	if err == nil {
		t.Error("expected error for non-existent session")
	}
}

// TestDaemonPendingPermissionQueue verifies that two rapid permission events
// both survive in the daemon's queue (the second must not overwrite the first).
// Regression test for: reading ~/.opencode and ~/.clank concurrently caused
// the first permission to be lost, hanging the agent.
func TestDaemonPendingPermissionQueue(t *testing.T) {
	t.Parallel()

	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "read two dirs",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitFor(t, 2*time.Second, "backend started", func() bool {
		return getBackend() != nil
	})
	b := getBackend()

	// Emit two permission events in quick succession.
	b.events <- agent.Event{
		Type:      agent.EventPermission,
		Timestamp: time.Now(),
		Data: agent.PermissionData{
			RequestID:   "perm-opencode",
			Tool:        "read",
			Description: "~/.opencode",
		},
	}
	b.events <- agent.Event{
		Type:      agent.EventPermission,
		Timestamp: time.Now(),
		Data: agent.PermissionData{
			RequestID:   "perm-clank",
			Tool:        "read",
			Description: "~/.clank",
		},
	}

	waitFor(t, 2*time.Second, "both permissions queued", func() bool {
		got, err := client.Session(info.ID).PendingPermissions(ctx)
		return err == nil && len(got) == 2
	})

	// Both permissions must be present.
	perms, err := client.Session(info.ID).PendingPermissions(ctx)
	if err != nil {
		t.Fatalf("GetPendingPermissions: %v", err)
	}
	if len(perms) != 2 {
		t.Fatalf("expected 2 pending permissions, got %d", len(perms))
	}
	if perms[0].RequestID != "perm-opencode" {
		t.Errorf("perms[0].RequestID = %q, want %q", perms[0].RequestID, "perm-opencode")
	}
	if perms[1].RequestID != "perm-clank" {
		t.Errorf("perms[1].RequestID = %q, want %q", perms[1].RequestID, "perm-clank")
	}

	// Reply to the first with allow — only the second should remain.
	if err := client.Session(info.ID).ReplyPermission(ctx, "perm-opencode", true); err != nil {
		t.Fatalf("ReplyPermission (first): %v", err)
	}
	perms, err = client.Session(info.ID).PendingPermissions(ctx)
	if err != nil {
		t.Fatalf("GetPendingPermissions after first reply: %v", err)
	}
	if len(perms) != 1 {
		t.Fatalf("expected 1 pending permission after first reply, got %d", len(perms))
	}
	if perms[0].RequestID != "perm-clank" {
		t.Errorf("remaining perm RequestID = %q, want %q", perms[0].RequestID, "perm-clank")
	}

	// Reply to the second with deny — the queue should be fully cleared.
	if err := client.Session(info.ID).ReplyPermission(ctx, "perm-clank", false); err != nil {
		t.Fatalf("ReplyPermission (second): %v", err)
	}
	perms, err = client.Session(info.ID).PendingPermissions(ctx)
	if err != nil {
		t.Fatalf("GetPendingPermissions after second reply: %v", err)
	}
	if len(perms) != 0 {
		t.Fatalf("expected 0 pending permissions after both replies, got %d", len(perms))
	}
}

// TestDaemonPendingPermissionRejectClearsQueue verifies that rejecting one
// permission clears the remaining queued prompts from the daemon, matching the
// backend behavior where a deny cancels the current permission batch.
func TestDaemonPendingPermissionRejectClearsQueue(t *testing.T) {
	t.Parallel()

	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "read two dirs",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitFor(t, 2*time.Second, "backend started", func() bool {
		return getBackend() != nil
	})
	b := getBackend()

	b.events <- agent.Event{
		Type:      agent.EventPermission,
		Timestamp: time.Now(),
		Data: agent.PermissionData{
			RequestID:   "perm-a",
			Tool:        "external_directory",
			Description: "/Users/test/a",
		},
	}
	b.events <- agent.Event{
		Type:      agent.EventPermission,
		Timestamp: time.Now(),
		Data: agent.PermissionData{
			RequestID:   "perm-b",
			Tool:        "external_directory",
			Description: "/Users/test/b",
		},
	}

	waitFor(t, 2*time.Second, "both deny-test permissions queued", func() bool {
		got, err := client.Session(info.ID).PendingPermissions(ctx)
		return err == nil && len(got) == 2
	})

	perms, err := client.Session(info.ID).PendingPermissions(ctx)
	if err != nil {
		t.Fatalf("GetPendingPermissions before deny: %v", err)
	}
	if len(perms) != 2 {
		t.Fatalf("expected 2 pending permissions before deny, got %d", len(perms))
	}

	if err := client.Session(info.ID).ReplyPermission(ctx, "perm-a", false); err != nil {
		t.Fatalf("ReplyPermission (deny): %v", err)
	}

	perms, err = client.Session(info.ID).PendingPermissions(ctx)
	if err != nil {
		t.Fatalf("GetPendingPermissions after deny: %v", err)
	}
	if len(perms) != 0 {
		t.Fatalf("expected 0 pending permissions after deny, got %d", len(perms))
	}
}
