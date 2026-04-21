package hub_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/hub"
	"github.com/acksell/clank/internal/store"
)

func TestDaemonCreateSession(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "Fix the bug",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if info.ID == "" {
		t.Error("expected non-empty session ID")
	}
	if info.Backend != agent.BackendOpenCode {
		t.Errorf("expected backend=opencode, got %s", info.Backend)
	}
	if info.GitRef.RemoteURL == "" || info.GitRef.RemoteURL != testRemoteURL {
		t.Errorf("expected git_ref.remote_url=%s, got %q", testRemoteURL, info.GitRef.RemoteURL)
	}
	if info.Hostname != "local" {
		t.Errorf("expected host_id=local, got %s", info.Hostname)
	}
	if info.Prompt != "Fix the bug" {
		t.Errorf("expected prompt='Fix the bug', got %s", info.Prompt)
	}
}

func TestDaemonCreateSessionValidation(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()

	// Missing backend.
	_, err := client.Sessions().Create(ctx, agent.StartRequest{
		GitRef: agent.GitRef{RemoteURL: testRemoteURL},
		Prompt: "test",
	})
	if err == nil {
		t.Error("expected error for missing backend")
	}

	// Missing project dir.
	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		Prompt:  "test",
	})
	if err == nil {
		t.Error("expected error for missing project_dir")
	}

	// Missing prompt.
	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
	})
	if err == nil {
		t.Error("expected error for missing prompt")
	}

	// Invalid backend.
	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: "invalid",
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test",
	})
	if err == nil {
		t.Error("expected error for invalid backend")
	}
}

func TestDaemonListSessions(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()

	// Initially empty.
	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}

	// Create two sessions.
	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "task a",
	})
	if err != nil {
		t.Fatalf("CreateSession a: %v", err)
	}

	_, err = client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendClaudeCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "task b",
	})
	if err != nil {
		t.Fatalf("CreateSession b: %v", err)
	}

	// Allow time for backends to start.
	time.Sleep(100 * time.Millisecond)

	sessions, err = client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestDaemonGetSession(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ID != info.ID {
		t.Errorf("expected ID=%s, got %s", info.ID, got.ID)
	}

	// Non-existent session.
	_, err = client.Session("nonexistent").Get(ctx)
	if err == nil {
		t.Error("expected error for non-existent session")
	}
}

func TestDaemonGetSessionMessages(t *testing.T) {
	t.Parallel()
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Set up history on the mock backend.
	b := getBackend()
	b.mu.Lock()
	b.history = []agent.MessageData{
		{
			Role:    "user",
			Content: "test prompt",
			Parts: []agent.Part{
				{ID: "p1", Type: agent.PartText, Text: "test prompt"},
			},
		},
		{
			Role: "assistant",
			Parts: []agent.Part{
				{ID: "p2", Type: agent.PartText, Text: "Here is my response"},
				{ID: "p3", Type: agent.PartToolCall, Tool: "bash", Status: agent.PartCompleted},
			},
		},
	}
	b.mu.Unlock()

	messages, err := client.Session(info.ID).Messages(ctx)
	if err != nil {
		t.Fatalf("GetSessionMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].Role != "user" {
		t.Errorf("expected first message role=user, got %s", messages[0].Role)
	}
	if messages[1].Role != "assistant" {
		t.Errorf("expected second message role=assistant, got %s", messages[1].Role)
	}
	if len(messages[1].Parts) != 2 {
		t.Errorf("expected 2 parts in assistant message, got %d", len(messages[1].Parts))
	}
	if messages[1].Parts[0].ID != "p2" || messages[1].Parts[0].Text != "Here is my response" {
		t.Errorf("unexpected first part: %+v", messages[1].Parts[0])
	}
	if messages[1].Parts[1].Tool != "bash" || messages[1].Parts[1].Status != agent.PartCompleted {
		t.Errorf("unexpected second part: %+v", messages[1].Parts[1])
	}
}

func TestDaemonGetSessionMessagesEmpty(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Backend returns nil history by default — should get empty array.
	messages, err := client.Session(info.ID).Messages(ctx)
	if err != nil {
		t.Fatalf("GetSessionMessages: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("expected 0 messages, got %d", len(messages))
	}
}

func TestDaemonGetSessionMessagesNotFound(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	_, err := client.Session("nonexistent").Messages(context.Background())
	if err == nil {
		t.Error("expected error for non-existent session")
	}
}

func TestDaemonSendMessage(t *testing.T) {
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
		Prompt:  "initial prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Wait for backend to start.
	time.Sleep(100 * time.Millisecond)

	err = client.Session(info.ID).Send(ctx, agent.SendMessageOpts{Text: "follow-up message"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Verify the backend received the message.
	b := mgr.getLatest()

	time.Sleep(50 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.messages) != 1 || b.messages[0] != "follow-up message" {
		t.Errorf("expected messages=[follow-up message], got %v", b.messages)
	}
}

func TestDaemonAbortSession(t *testing.T) {
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

	time.Sleep(100 * time.Millisecond)

	err = client.Session(info.ID).Abort(ctx)
	if err != nil {
		t.Fatalf("AbortSession: %v", err)
	}

	b := mgr.getLatest()

	time.Sleep(50 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.aborted {
		t.Error("expected backend to be aborted")
	}
}

func TestDaemonRevertSession(t *testing.T) {
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

	time.Sleep(100 * time.Millisecond)

	err = client.Session(info.ID).Revert(ctx, "msg-abc-123")
	if err != nil {
		t.Fatalf("RevertSession: %v", err)
	}

	b := mgr.getLatest()

	time.Sleep(50 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.reverted {
		t.Error("expected backend to be reverted")
	}
	if b.revertedMessageID != "msg-abc-123" {
		t.Errorf("expected revertedMessageID=msg-abc-123, got %s", b.revertedMessageID)
	}
}

func TestDaemonRevertSessionMissingMessageID(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Empty message_id should return an error.
	err = client.Session(info.ID).Revert(ctx, "")
	if err == nil {
		t.Error("expected error for empty message_id")
	}
}

func TestDaemonRevertSessionNotFound(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	err := client.Session("nonexistent").Revert(context.Background(), "msg-123")
	if err == nil {
		t.Error("expected error reverting non-existent session")
	}
}

func TestDaemonForkSession(t *testing.T) {
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
		Prompt:  "original session",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	forked, err := client.Session(info.ID).Fork(ctx, "msg-fork-point")
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}

	// The forked session should have a different daemon ID.
	if forked.ID == info.ID {
		t.Error("forked session should have a different ID")
	}
	if forked.ID == "" {
		t.Error("forked session ID should not be empty")
	}
	if forked.ExternalID != "forked-external-id" {
		t.Errorf("expected ExternalID=forked-external-id, got %s", forked.ExternalID)
	}
	if forked.Title != "Forked session" {
		t.Errorf("expected Title=%q, got %q", "Forked session", forked.Title)
	}
	if agent.RepoKey(forked.GitRef) != agent.RepoKey(info.GitRef) {
		t.Errorf("expected GitRef=%s, got %s", agent.RepoKey(info.GitRef), agent.RepoKey(forked.GitRef))
	}
	if forked.GitRef.WorktreeBranch != info.GitRef.WorktreeBranch {
		t.Errorf("expected WorktreeBranch=%s, got %s", info.GitRef.WorktreeBranch, forked.GitRef.WorktreeBranch)
	}
	if forked.Backend != info.Backend {
		t.Errorf("expected Backend=%s, got %s", info.Backend, forked.Backend)
	}

	// Verify the original backend received the fork call.
	origBackend := mgr.all[0]
	origBackend.mu.Lock()
	defer origBackend.mu.Unlock()
	if !origBackend.forked {
		t.Error("expected original backend to be forked")
	}
	if origBackend.forkedMessageID != "msg-fork-point" {
		t.Errorf("expected forkedMessageID=msg-fork-point, got %s", origBackend.forkedMessageID)
	}
}

func TestDaemonForkSessionEmptyMessageID(t *testing.T) {
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
		Prompt:  "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Empty message_id should fork the entire session (not return an error).
	forked, err := client.Session(info.ID).Fork(ctx, "")
	if err != nil {
		t.Fatalf("ForkSession with empty messageID: %v", err)
	}
	if forked.ID == "" {
		t.Error("forked session ID should not be empty")
	}

	// Verify the backend received an empty messageID.
	origBackend := mgr.all[0]
	origBackend.mu.Lock()
	defer origBackend.mu.Unlock()
	if !origBackend.forked {
		t.Error("expected original backend to be forked")
	}
	if origBackend.forkedMessageID != "" {
		t.Errorf("expected empty forkedMessageID, got %s", origBackend.forkedMessageID)
	}
}

func TestDaemonForkSessionNotFound(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	_, err := client.Session("nonexistent").Fork(context.Background(), "msg-123")
	if err == nil {
		t.Error("expected error forking non-existent session")
	}
}

func TestDaemonDeleteSession(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	err = client.Session(info.ID).Delete(ctx)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// Session should be gone.
	_, err = client.Session(info.ID).Get(ctx)
	if err == nil {
		t.Error("expected error getting deleted session")
	}

	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions after delete, got %d", len(sessions))
	}
}

func TestDaemonSendMessageToNonexistentSession(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	err := client.Session("nonexistent").Send(ctx, agent.SendMessageOpts{Text: "hello"})
	if err == nil {
		t.Error("expected error sending to non-existent session")
	}
}

func TestDaemonSendEmptyMessage(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	err = client.Session(info.ID).Send(ctx, agent.SendMessageOpts{Text: ""})
	if err == nil {
		t.Error("expected error sending empty message")
	}
}

func TestDaemonDeleteNonexistentSession(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	err := client.Session("nonexistent").Delete(ctx)
	if err == nil {
		t.Error("expected error deleting non-existent session")
	}
}

func TestDaemonSessionInfoUnread(t *testing.T) {
	info := agent.SessionInfo{
		CreatedAt: time.Now().Add(-1 * time.Hour),
		UpdatedAt: time.Now(),
	}

	// No LastReadAt set — should be unread.
	if !info.Unread() {
		t.Error("expected session with zero LastReadAt to be unread")
	}

	// LastReadAt before UpdatedAt — still unread.
	info.LastReadAt = time.Now().Add(-30 * time.Minute)
	if !info.Unread() {
		t.Error("expected session with old LastReadAt to be unread")
	}

	// LastReadAt after UpdatedAt — read.
	info.LastReadAt = time.Now().Add(1 * time.Minute)
	if info.Unread() {
		t.Error("expected session with recent LastReadAt to be read")
	}
}

func TestDaemonMarkSessionRead(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()

	// Create a session.
	created, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Allow the backend's startup status events to flow through SSE
	// before we mark the session read; otherwise a late StatusChange
	// can bump UpdatedAt past LastReadAt and re-mark it unread.
	time.Sleep(100 * time.Millisecond)

	// Newly created session should be unread (LastReadAt is zero).
	info, err := client.Session(created.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !info.Unread() {
		t.Error("expected new session to be unread")
	}

	// Mark the session as read.
	if err := client.Session(created.ID).MarkRead(ctx); err != nil {
		t.Fatalf("MarkSessionRead: %v", err)
	}

	// Session should now be read.
	info, err = client.Session(created.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession after mark read: %v", err)
	}
	if info.Unread() {
		t.Error("expected session to be read after MarkSessionRead")
	}
	if info.LastReadAt.IsZero() {
		t.Error("expected LastReadAt to be set")
	}
}

func TestDaemonMarkSessionReadNotFound(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	err := client.Session("nonexistent-id").MarkRead(context.Background())
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestDaemonMarkSessionReadThenUpdate(t *testing.T) {
	t.Parallel()
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()

	created, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Drain the startup status events so MarkSessionRead races with
	// nothing — see TestDaemonMarkSessionRead for the same caveat.
	time.Sleep(100 * time.Millisecond)

	// Mark as read.
	if err := client.Session(created.ID).MarkRead(ctx); err != nil {
		t.Fatalf("MarkSessionRead: %v", err)
	}

	// Verify it's read.
	info, err := client.Session(created.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if info.Unread() {
		t.Fatal("expected session to be read")
	}

	// Emit a status change via the backend to bump UpdatedAt.
	backend := getBackend()
	backend.events <- agent.Event{
		Type:      agent.EventStatusChange,
		Timestamp: time.Now(),
		Data: agent.StatusChangeData{
			OldStatus: agent.StatusBusy,
			NewStatus: agent.StatusIdle,
		},
	}

	// Give the event relay a moment to propagate.
	time.Sleep(200 * time.Millisecond)

	// Session should be unread again because UpdatedAt was bumped.
	info, err = client.Session(created.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession after status change: %v", err)
	}
	if !info.Unread() {
		t.Error("expected session to be unread after status change")
	}
}

// --- Event Round-Trip Tests ---
//
// These tests verify that Event.Data survives the full path:
//   backend emit -> daemon broadcast -> SSE serialize -> client parse -> concrete type
//
// This is the critical path for the TUI to receive properly-typed events.

// testDaemonWithBackendAccess is like testDaemon but returns a function to get
// the most recently created mock backend.
func TestDaemonTitleUpdateOnSession(t *testing.T) {
	t.Parallel()
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()

	created, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "Fix the login bug",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify title is initially empty.
	info, err := client.Session(created.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if info.Title != "" {
		t.Errorf("expected empty title initially, got %q", info.Title)
	}

	// Emit a title change via the backend.
	backend := getBackend()
	backend.events <- agent.Event{
		Type:      agent.EventTitleChange,
		Timestamp: time.Now(),
		Data: agent.TitleChangeData{
			Title: "Fix authentication bug in login flow",
		},
	}

	// Give the event relay a moment to propagate.
	time.Sleep(200 * time.Millisecond)

	// Session should now have the title.
	info, err = client.Session(created.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession after title change: %v", err)
	}
	if info.Title != "Fix authentication bug in login flow" {
		t.Errorf("title = %q, want %q", info.Title, "Fix authentication bug in login flow")
	}
}

// TestDaemonTitleVisibleInList verifies that the title field is returned
// in the session list after being updated by a backend event.
func TestDaemonTitleVisibleInList(t *testing.T) {
	t.Parallel()
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "Fix the login bug",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Emit a title change via the backend.
	backend := getBackend()
	backend.events <- agent.Event{
		Type:      agent.EventTitleChange,
		Timestamp: time.Now(),
		Data: agent.TitleChangeData{
			Title: "Fix authentication bug in login flow",
		},
	}

	time.Sleep(200 * time.Millisecond)

	// Title should be visible in session list.
	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Title != "Fix authentication bug in login flow" {
		t.Errorf("title in list = %q, want %q", sessions[0].Title, "Fix authentication bug in login flow")
	}
}

// --- Helpers ---

func TestDaemonAgentStoredOnSession(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()

	// Create session with agent specified.
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test with agent",
		Agent:   "plan",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify agent is stored on session info.
	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Agent != "plan" {
		t.Errorf("session agent = %q, want %q", got.Agent, "plan")
	}

	// Send a message with a different agent — should update the session's agent.
	err = client.Session(info.ID).Send(ctx, agent.SendMessageOpts{Text: "follow up", Agent: "build"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	got, err = client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession after SendMessage: %v", err)
	}
	if got.Agent != "build" {
		t.Errorf("session agent after SendMessage = %q, want %q", got.Agent, "build")
	}
}

// --- Discover + Historical Session Tests ---

// testDaemonWithDiscover creates a daemon with a mock SessionDiscoverer and
// a backend manager that records created backends. Returns the daemon, client,
// a function to get the latest backend, and a cleanup function.
func TestDiscoverSessionsAddsHistoricalSessions(t *testing.T) {
	t.Parallel()
	snapshots := []agent.SessionSnapshot{
		{
			ID:        "oc-session-aaa",
			Title:     "Fix login bug",
			Directory: "/tmp/project-alpha",
			CreatedAt: time.Now().Add(-2 * time.Hour),
			UpdatedAt: time.Now().Add(-1 * time.Hour),
		},
		{
			ID:        "oc-session-bbb",
			Title:     "Add dark mode",
			Directory: "/tmp/project-beta",
			CreatedAt: time.Now().Add(-3 * time.Hour),
			UpdatedAt: time.Now().Add(-2 * time.Hour),
		},
	}

	_, client, _, _, cleanup := testDaemonWithDiscover(t, snapshots)
	defer cleanup()

	ctx := context.Background()

	// Trigger discovery.
	if err := client.Sessions().Discover(ctx, "/tmp/project-alpha"); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	// Both sessions should now appear in the list.
	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	// Verify the sessions have the right data.
	titles := map[string]bool{}
	for _, s := range sessions {
		titles[s.Title] = true
		if s.ExternalID == "" {
			t.Errorf("expected non-empty ExternalID for session %q", s.Title)
		}
		if s.Status != agent.StatusIdle {
			t.Errorf("expected status=idle for discovered session, got %s", s.Status)
		}
		if s.Backend != agent.BackendOpenCode {
			t.Errorf("expected backend=opencode, got %s", s.Backend)
		}
	}
	if !titles["Fix login bug"] || !titles["Add dark mode"] {
		t.Errorf("unexpected titles: %v", titles)
	}
}

func TestDiscoverSessionsDeduplicates(t *testing.T) {
	t.Parallel()
	snapshots := []agent.SessionSnapshot{
		{
			ID:        "oc-session-xxx",
			Title:     "Refactor auth",
			Directory: "/tmp/project-x",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			UpdatedAt: time.Now().Add(-30 * time.Minute),
		},
	}

	_, client, _, _, cleanup := testDaemonWithDiscover(t, snapshots)
	defer cleanup()

	ctx := context.Background()

	// Discover twice.
	if err := client.Sessions().Discover(ctx, "/tmp/project-x"); err != nil {
		t.Fatalf("DiscoverSessions (1st): %v", err)
	}
	if err := client.Sessions().Discover(ctx, "/tmp/project-x"); err != nil {
		t.Fatalf("DiscoverSessions (2nd): %v", err)
	}

	// Should still only have 1 session (not duplicated).
	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("expected 1 session after double-discover, got %d", len(sessions))
	}
}

func TestDiscoverSessionsSkipsManagedSessions(t *testing.T) {
	t.Parallel()

	s := hub.New()

	// The mock backend returns "oc-real-session" as its SessionID, mimicking
	// what happens when OpenCodeBackend.Start() creates a real session.
	discMgr := &mockDiscovererManager{
		snapshots: []agent.SessionSnapshot{
			{
				ID:        "oc-real-session",
				Title:     "Already running",
				Directory: "/tmp/project-z",
				CreatedAt: time.Now().Add(-1 * time.Hour),
				UpdatedAt: time.Now(),
			},
		},
	}
	discMgr.create = func(inv agent.BackendInvocation) *mockBackend {
		b := newMockBackend()
		b.sessionID = "oc-real-session"
		return b
	}
	s.BackendManagers[agent.BackendOpenCode] = discMgr
	s.BackendManagers[agent.BackendClaudeCode] = &discMgr.mockBackendManager

	client, _, cleanup := startHubOnSocket(t, s)
	defer cleanup()
	registerTestRepo(t, s)

	ctx := context.Background()

	// Create a real session first. runBackend will set ExternalID to "oc-real-session".
	_, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "do stuff",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Wait for runBackend to capture the ExternalID.
	time.Sleep(200 * time.Millisecond)

	// Now discover — should NOT create a duplicate.
	if err := client.Sessions().Discover(ctx, "/tmp/project-z"); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("expected 1 session (no duplicate from discover), got %d", len(sessions))
	}
}

func TestHistoricalSessionMessagesActivatesBackend(t *testing.T) {
	t.Parallel()
	snapshots := []agent.SessionSnapshot{
		{
			ID:        "oc-hist-msg",
			Title:     "Old session",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			UpdatedAt: time.Now().Add(-30 * time.Minute),
		},
	}

	_, client, getBackend, repoDir, cleanup := testDaemonWithDiscover(t, snapshots)
	defer cleanup()
	// Snapshot must point at a real git repo so the discover path can
	// recover the GitRef via `git remote get-url origin`. Mutating
	// the slice is safe — the discoverer holds the same backing array.
	snapshots[0].Directory = repoDir

	ctx := context.Background()

	// Discover the historical session.
	if err := client.Sessions().Discover(ctx, repoDir); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	// Find the discovered session's daemon ID.
	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	sessionID := sessions[0].ID

	// Backend should be nil before fetching messages — no getBackend() yet.
	if getBackend() != nil {
		t.Fatal("expected no backend before message fetch")
	}

	// Fetch messages — this should trigger lazy backend activation.
	messages, err := client.Session(sessionID).Messages(ctx)
	if err != nil {
		t.Fatalf("GetSessionMessages: %v", err)
	}
	// Default mock returns nil history → empty array from the handler.
	if len(messages) != 0 {
		t.Errorf("expected 0 messages from fresh mock, got %d", len(messages))
	}

	// Backend should now be activated.
	b := getBackend()
	if b == nil {
		t.Fatal("expected backend to be activated after message fetch")
	}
	// The backend should have been created with the correct external session ID.
	if b.sessionID != "oc-hist-msg" {
		t.Errorf("backend sessionID = %q, want %q", b.sessionID, "oc-hist-msg")
	}
}

func TestHistoricalSessionResumeActivatesBackend(t *testing.T) {
	t.Parallel()
	snapshots := []agent.SessionSnapshot{
		{
			ID:        "oc-hist-resume",
			Title:     "Resume me",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			UpdatedAt: time.Now().Add(-30 * time.Minute),
		},
	}

	_, client, getBackend, repoDir, cleanup := testDaemonWithDiscover(t, snapshots)
	defer cleanup()
	// Snapshot must point at a real git repo so the discover path can
	// recover the GitRef via `git remote get-url origin`.
	snapshots[0].Directory = repoDir

	ctx := context.Background()

	// Discover the historical session.
	if err := client.Sessions().Discover(ctx, repoDir); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	sessionID := sessions[0].ID

	// No backend yet.
	if getBackend() != nil {
		t.Fatal("expected no backend before resume")
	}

	// Send a follow-up message — this triggers resume (activateBackend + runBackend).
	err = client.Session(sessionID).Send(ctx, agent.SendMessageOpts{Text: "continue from here"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Give runBackend time to start.
	time.Sleep(200 * time.Millisecond)

	b := getBackend()
	if b == nil {
		t.Fatal("expected backend to be activated after resume")
	}
	if b.sessionID != "oc-hist-resume" {
		t.Errorf("backend sessionID = %q, want %q", b.sessionID, "oc-hist-resume")
	}

	// Backend.Start() should have been called (runBackend calls it).
	b.mu.Lock()
	started := b.started
	b.mu.Unlock()
	if !started {
		t.Error("expected backend.Start() to have been called for resume")
	}
}

// --- SetVisibility Tests ---

func TestDaemonSetVisibilityDone(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark as done.
	if err := client.Session(info.ID).SetVisibility(ctx, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility(done): %v", err)
	}

	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Visibility != agent.VisibilityDone {
		t.Errorf("visibility = %q, want %q", got.Visibility, agent.VisibilityDone)
	}
	if !got.Hidden() {
		t.Error("expected session to be hidden after marking done")
	}
}

func TestDaemonSetVisibilityArchived(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark as archived.
	if err := client.Session(info.ID).SetVisibility(ctx, agent.VisibilityArchived); err != nil {
		t.Fatalf("SetVisibility(archived): %v", err)
	}

	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Visibility != agent.VisibilityArchived {
		t.Errorf("visibility = %q, want %q", got.Visibility, agent.VisibilityArchived)
	}
	if !got.Hidden() {
		t.Error("expected session to be hidden after archiving")
	}
}

func TestDaemonSetVisibilityBackToVisible(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark done, then revert to visible.
	if err := client.Session(info.ID).SetVisibility(ctx, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility(done): %v", err)
	}
	if err := client.Session(info.ID).SetVisibility(ctx, agent.VisibilityVisible); err != nil {
		t.Fatalf("SetVisibility(visible): %v", err)
	}

	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Visibility != agent.VisibilityVisible {
		t.Errorf("visibility = %q, want %q", got.Visibility, agent.VisibilityVisible)
	}
	if got.Hidden() {
		t.Error("expected session to not be hidden after reverting to visible")
	}
}

func TestDaemonUnarchiveSession(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Archive the session.
	if err := client.Session(info.ID).SetVisibility(ctx, agent.VisibilityArchived); err != nil {
		t.Fatalf("SetVisibility(archived): %v", err)
	}

	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession after archive: %v", err)
	}
	if got.Visibility != agent.VisibilityArchived {
		t.Errorf("visibility = %q, want %q", got.Visibility, agent.VisibilityArchived)
	}
	if !got.Hidden() {
		t.Error("expected session to be hidden after archiving")
	}

	// Unarchive the session.
	if err := client.Session(info.ID).SetVisibility(ctx, agent.VisibilityVisible); err != nil {
		t.Fatalf("SetVisibility(visible): %v", err)
	}

	got, err = client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession after unarchive: %v", err)
	}
	if got.Visibility != agent.VisibilityVisible {
		t.Errorf("visibility = %q, want %q", got.Visibility, agent.VisibilityVisible)
	}
	if got.Hidden() {
		t.Error("expected session to not be hidden after unarchiving")
	}
}

func TestDaemonSetVisibilityInvalid(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Invalid visibility value should fail.
	err = client.Session(info.ID).SetVisibility(ctx, agent.SessionVisibility("invalid"))
	if err == nil {
		t.Error("expected error for invalid visibility value")
	}
}

func TestDaemonSetVisibilityNotFound(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	err := client.Session("nonexistent-id").SetVisibility(context.Background(), agent.VisibilityDone)
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestDaemonSendMessageClearsDoneVisibility(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark session as done.
	if err := client.Session(info.ID).SetVisibility(ctx, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility(done): %v", err)
	}
	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Visibility != agent.VisibilityDone {
		t.Fatalf("visibility = %q, want %q", got.Visibility, agent.VisibilityDone)
	}

	// Send a follow-up message to the done session.
	if err := client.Session(info.ID).Send(ctx, agent.SendMessageOpts{Text: "follow up"}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Visibility should be reset to visible.
	got, err = client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession after SendMessage: %v", err)
	}
	if got.Visibility != agent.VisibilityVisible {
		t.Errorf("visibility after SendMessage = %q, want %q (empty/visible)", got.Visibility, agent.VisibilityVisible)
	}
}

func TestDaemonSendMessageClearsArchivedVisibility(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark session as archived.
	if err := client.Session(info.ID).SetVisibility(ctx, agent.VisibilityArchived); err != nil {
		t.Fatalf("SetVisibility(archived): %v", err)
	}
	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Visibility != agent.VisibilityArchived {
		t.Fatalf("visibility = %q, want %q", got.Visibility, agent.VisibilityArchived)
	}

	// Send a follow-up message to the archived session.
	if err := client.Session(info.ID).Send(ctx, agent.SendMessageOpts{Text: "follow up"}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Visibility should be reset to visible.
	got, err = client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession after SendMessage: %v", err)
	}
	if got.Visibility != agent.VisibilityVisible {
		t.Errorf("visibility after SendMessage = %q, want %q (empty/visible)", got.Visibility, agent.VisibilityVisible)
	}
}

// --- SetDraft Tests ---

func TestDaemonSetDraft(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Set a draft.
	if err := client.Session(info.ID).SetDraft(ctx, "work in progress"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}

	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Draft != "work in progress" {
		t.Errorf("draft = %q, want %q", got.Draft, "work in progress")
	}
}

func TestDaemonSetDraftClear(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Set then clear.
	if err := client.Session(info.ID).SetDraft(ctx, "draft text"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}
	if err := client.Session(info.ID).SetDraft(ctx, ""); err != nil {
		t.Fatalf("SetDraft(clear): %v", err)
	}

	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Draft != "" {
		t.Errorf("draft = %q, want empty", got.Draft)
	}
}

func TestDaemonSetDraftNotFound(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	err := client.Session("nonexistent-id").SetDraft(context.Background(), "draft")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestDaemonDraftVisibleInList(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := client.Session(info.ID).SetDraft(ctx, "my draft"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}

	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Draft != "my draft" {
		t.Errorf("draft in list = %q, want %q", sessions[0].Draft, "my draft")
	}
}

func TestDaemonDraftClearedOnSendMessage(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Set a draft.
	if err := client.Session(info.ID).SetDraft(ctx, "my draft"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}

	// Send a message — should clear the draft.
	if err := client.Session(info.ID).Send(ctx, agent.SendMessageOpts{Text: "real message"}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Draft != "" {
		t.Errorf("draft = %q after send, want empty", got.Draft)
	}
}

// testDaemonWithStore creates a daemon backed by a real SQLite store in the
// given dir. If dir is "", a fresh temp dir is created. Returns the daemon,
// client, store, paths, and cleanup func. The caller must call cleanup to
// stop the hub.
func TestDaemonSearchSessions(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	dbPath := filepath.Join(dir, "test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	// Seed sessions with distinct timestamps so we can verify date ordering
	// and time filtering.
	now := time.Now().Truncate(time.Millisecond)
	for _, info := range []agent.SessionInfo{
		{
			ID: "ses-s1", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			GitRef: agent.GitRef{RemoteURL: "git@github.com:acme/myproject.git"},
			Title:  "Fix authentication bug", Prompt: "fix login",
			CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour),
		},
		{
			ID: "ses-s2", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			GitRef: agent.GitRef{RemoteURL: "git@github.com:acme/myproject.git"},
			Title:  "Add dark mode", Prompt: "implement dark mode toggle",
			CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour),
		},
		{
			ID: "ses-s3", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			GitRef: agent.GitRef{RemoteURL: "git@github.com:acme/otherproj.git"},
			Title:  "Refactor database layer", Prompt: "clean up db queries",
			CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := st.UpsertSession(info); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	}

	s := hub.New()
	s.Store = st
	mgr := newMockBackendManager()
	s.BackendManagers[agent.BackendOpenCode] = mgr
	s.BackendManagers[agent.BackendClaudeCode] = mgr

	client, _, cleanup := startHubOnSocket(t, s)
	defer cleanup()
	registerTestRepo(t, s)

	ctx := context.Background()
	search := func(q string) agent.SearchParams { return agent.SearchParams{Query: q} }

	// --- Substring AND matching ---

	// Substring match: "authentication" appears in ses-s1 title.
	results, err := client.Sessions().Search(ctx, search("authentication"))
	if err != nil {
		t.Fatalf("SearchSessions(authentication): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'authentication', got %d", len(results))
	}
	if results[0].ID != "ses-s1" {
		t.Errorf("expected ses-s1, got %s", results[0].ID)
	}

	// Multi-word AND: both "dark" and "toggle" must appear.
	results, err = client.Sessions().Search(ctx, search("dark toggle"))
	if err != nil {
		t.Fatalf("SearchSessions(dark toggle): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'dark toggle', got %d", len(results))
	}
	if results[0].ID != "ses-s2" {
		t.Errorf("expected ses-s2, got %s", results[0].ID)
	}

	// Multi-word AND where one term doesn't match: "dark queries" returns nothing.
	results, err = client.Sessions().Search(ctx, search("dark queries"))
	if err != nil {
		t.Fatalf("SearchSessions(dark queries): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for 'dark queries', got %d", len(results))
	}

	// Case insensitive: "DATABASE" matches "database" in ses-s3.
	results, err = client.Sessions().Search(ctx, search("DATABASE"))
	if err != nil {
		t.Fatalf("SearchSessions(DATABASE): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'DATABASE', got %d", len(results))
	}
	if results[0].ID != "ses-s3" {
		t.Errorf("expected ses-s3, got %s", results[0].ID)
	}

	// --- OR matching ---

	// Pipe-separated OR: "auth|dark" matches ses-s1 and ses-s2.
	results, err = client.Sessions().Search(ctx, search("auth|dark"))
	if err != nil {
		t.Fatalf("SearchSessions(auth|dark): %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for 'auth|dark', got %d", len(results))
	}
	// Most recent first.
	if results[0].ID != "ses-s2" {
		t.Errorf("expected ses-s2 first, got %s", results[0].ID)
	}
	if results[1].ID != "ses-s1" {
		t.Errorf("expected ses-s1 second, got %s", results[1].ID)
	}

	// OR with AND groups: "auth bug|database layer" matches ses-s1 and ses-s3.
	results, err = client.Sessions().Search(ctx, search("auth bug|database layer"))
	if err != nil {
		t.Fatalf("SearchSessions(auth bug|database layer): %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for 'auth bug|database layer', got %d", len(results))
	}
	if results[0].ID != "ses-s3" {
		t.Errorf("expected ses-s3 first (most recent), got %s", results[0].ID)
	}
	if results[1].ID != "ses-s1" {
		t.Errorf("expected ses-s1 second (oldest), got %s", results[1].ID)
	}

	// OR where one branch matches nothing: "xyznotfound|dark" matches only ses-s2.
	results, err = client.Sessions().Search(ctx, search("xyznotfound|dark"))
	if err != nil {
		t.Fatalf("SearchSessions(xyznotfound|dark): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'xyznotfound|dark', got %d", len(results))
	}
	if results[0].ID != "ses-s2" {
		t.Errorf("expected ses-s2, got %s", results[0].ID)
	}

	// --- Date ordering ---

	// "myproject" matches two sessions; most recent UpdatedAt should come first.
	results, err = client.Sessions().Search(ctx, search("myproject"))
	if err != nil {
		t.Fatalf("SearchSessions(myproject): %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for 'myproject', got %d", len(results))
	}
	if results[0].ID != "ses-s2" {
		t.Errorf("expected ses-s2 first (more recent), got %s", results[0].ID)
	}
	if results[1].ID != "ses-s1" {
		t.Errorf("expected ses-s1 second (older), got %s", results[1].ID)
	}

	// --- Time filtering ---

	// Since 2 hours ago: should exclude ses-s1 (48h ago), include ses-s2 and ses-s3.
	results, err = client.Sessions().Search(ctx, agent.SearchParams{
		Since: now.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("SearchSessions(since=2h): %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for since=2h, got %d", len(results))
	}
	if results[0].ID != "ses-s3" {
		t.Errorf("expected ses-s3 first, got %s", results[0].ID)
	}

	// Until 30 minutes ago: should include ses-s1 (48h ago) and ses-s2 (1h ago),
	// exclude ses-s3 (now).
	results, err = client.Sessions().Search(ctx, agent.SearchParams{
		Until: now.Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("SearchSessions(until=30m): %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for until=30m, got %d", len(results))
	}
	if results[0].ID != "ses-s2" {
		t.Errorf("expected ses-s2 first, got %s", results[0].ID)
	}
	if results[1].ID != "ses-s1" {
		t.Errorf("expected ses-s1 second, got %s", results[1].ID)
	}

	// Since + Until window: 3h ago to 30min ago — only ses-s2 (1h ago).
	results, err = client.Sessions().Search(ctx, agent.SearchParams{
		Since: now.Add(-3 * time.Hour),
		Until: now.Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("SearchSessions(since=3h,until=30m): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for since=3h+until=30m, got %d", len(results))
	}
	if results[0].ID != "ses-s2" {
		t.Errorf("expected ses-s2, got %s", results[0].ID)
	}

	// --- Combined query + time filter ---

	// "myproject" with since=2h: only ses-s2 (ses-s1 is 48h old).
	results, err = client.Sessions().Search(ctx, agent.SearchParams{
		Query: "myproject",
		Since: now.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("SearchSessions(myproject,since=2h): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for myproject+since=2h, got %d", len(results))
	}
	if results[0].ID != "ses-s2" {
		t.Errorf("expected ses-s2, got %s", results[0].ID)
	}

	// --- No match ---

	results, err = client.Sessions().Search(ctx, search("xyznotfound"))
	if err != nil {
		t.Fatalf("SearchSessions(xyznotfound): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for 'xyznotfound', got %d", len(results))
	}
}

func TestDaemonSearchSessionsAllParamsEmpty(t *testing.T) {
	t.Parallel()

	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()

	// All params empty should return an error.
	_, err := client.Sessions().Search(ctx, agent.SearchParams{})
	if err == nil {
		t.Fatal("expected error when all search params are empty")
	}
}

func TestDaemonSearchSessionsVisibility(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	dbPath := filepath.Join(dir, "test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	for _, info := range []agent.SessionInfo{
		{
			ID: "ses-active", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			Title: "active session", Prompt: "do stuff",
			Visibility: agent.VisibilityVisible,
			CreatedAt:  now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour),
		},
		{
			ID: "ses-done", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			Title: "done session", Prompt: "finished task",
			Visibility: agent.VisibilityDone,
			CreatedAt:  now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
		},
		{
			ID: "ses-archived", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			Title: "archived session", Prompt: "old stuff",
			Visibility: agent.VisibilityArchived,
			CreatedAt:  now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour),
		},
	} {
		if err := st.UpsertSession(info); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	}

	s := hub.New()
	s.Store = st
	mgr := newMockBackendManager()
	s.BackendManagers[agent.BackendOpenCode] = mgr
	s.BackendManagers[agent.BackendClaudeCode] = mgr

	client, _, cleanup := startHubOnSocket(t, s)
	defer cleanup()
	registerTestRepo(t, s)

	ctx := context.Background()

	// Default visibility (empty): only active sessions. Must provide at
	// least one search param for the HTTP handler, so use a broad query.
	results, err := client.Sessions().Search(ctx, agent.SearchParams{Query: "session"})
	if err != nil {
		t.Fatalf("SearchSessions(default visibility): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 active session, got %d", len(results))
	}
	if results[0].ID != "ses-active" {
		t.Errorf("expected ses-active, got %s", results[0].ID)
	}

	// Visibility "all": returns all three sessions.
	results, err = client.Sessions().Search(ctx, agent.SearchParams{
		Query:      "session",
		Visibility: agent.VisibilityAll,
	})
	if err != nil {
		t.Fatalf("SearchSessions(visibility=all): %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 sessions for visibility=all, got %d", len(results))
	}

	// Visibility "done": only done sessions.
	results, err = client.Sessions().Search(ctx, agent.SearchParams{
		Query:      "session",
		Visibility: agent.VisibilityDone,
	})
	if err != nil {
		t.Fatalf("SearchSessions(visibility=done): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 done session, got %d", len(results))
	}
	if results[0].ID != "ses-done" {
		t.Errorf("expected ses-done, got %s", results[0].ID)
	}

	// Visibility "archived": only archived sessions.
	results, err = client.Sessions().Search(ctx, agent.SearchParams{
		Query:      "session",
		Visibility: agent.VisibilityArchived,
	})
	if err != nil {
		t.Fatalf("SearchSessions(visibility=archived): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 archived session, got %d", len(results))
	}
	if results[0].ID != "ses-archived" {
		t.Errorf("expected ses-archived, got %s", results[0].ID)
	}
}
