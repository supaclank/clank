package hub_test

import (
	"context"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestPersistence_ExternalIDPersistedWhileStartStillRunning is a regression
// test backed by production evidence (see daemon.log analysis):
//
//   - 24 Claude session rows in the user's DB have external_id="" while
//     having project_dir populated correctly. So the metadata write
//     path works, but ExternalID is consistently missing.
//   - The most recent broken row was created at T+0s, updated at T+3s,
//     then never updated again. The persisted ExternalID stayed "".
//   - On reopen after a daemon restart, Messages() correctly returns
//     nil because there's no session ID to resume → TUI hangs on
//     "Waiting for agent output...".
//
// Root cause: runBackend persists ExternalID only AFTER backend.OpenAndSend()
// returns (sessions.go ~line 466). For Claude, SessionID is learned
// from the SystemMessage init event arriving DURING OpenAndSend(). If
// OpenAndSend takes a long time (LLM still streaming) and the daemon is
// killed before it returns, the in-memory sessionID is lost — the row
// stays at ExternalID="" forever.
//
// The fix: persist ExternalID as soon as the backend learns it (in the
// event relay loop), not when OpenAndSend returns.
func TestPersistence_ExternalIDPersistedWhileStartStillRunning(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	d, client, _, _, _, cleanup := testDaemonWithStore(t, dir)
	defer cleanup()

	ctx := context.Background()

	// Override the backend manager: created backend learns its
	// sessionID asynchronously (simulating the Claude init message),
	// and OpenAndSend() blocks until the test signals — simulating an
	// LLM response that hasn't completed when the daemon dies.
	mgr := newMockBackendManager()
	startReturned := make(chan struct{})
	mgr.create = func(inv agent.BackendInvocation) *mockBackend {
		b := newMockBackend()
		b.sessionID = "" // not yet known
		b.onOpenAndSend = func(ctx context.Context, opts agent.SendMessageOpts) error {
			t.Logf("DEBUG mock onOpenAndSend entered (text=%q)", opts.Text)
			// Simulate "init system message arrives ~20ms into Start()",
			// followed by a content event (in real Claude, the init
			// message is always followed by streaming content blocks
			// emitted via Events()). The relay loop must observe
			// SessionID() going non-empty on one of those events.
			go func() {
				time.Sleep(20 * time.Millisecond)
				b.mu.Lock()
				b.sessionID = "real-backend-session-id-123"
				b.mu.Unlock()
				t.Log("DEBUG mock set sessionID, emitting content event with ExternalID stamped")
				// Backends stamp Event.ExternalID on every emit once
				// they know it (see Event.ExternalID docstring). The
				// hub captures it from the next event that flows.
				b.events <- agent.Event{
					Type:       agent.EventMessage,
					ExternalID: "real-backend-session-id-123",
					Timestamp:  time.Now(),
					Data:       agent.MessageData{Role: "assistant"},
				}
				t.Log("DEBUG mock event sent")
			}()
			<-startReturned // never returns during the test body
			return nil
		}
		return b
	}
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr
	defer close(startReturned)

	info, err := client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: testRemoteURL},
		Prompt:  "do stuff",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Wait for the in-memory ExternalID to be set. This will only
	// succeed once the fix is in place — the relay loop must observe
	// SessionID() going non-empty and call persistSession.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := client.Session(info.ID).Get(ctx)
		if err == nil && got.ExternalID == "real-backend-session-id-123" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := client.Session(info.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExternalID != "real-backend-session-id-123" {
		t.Fatalf("in-memory ExternalID not populated while OpenAndSend still blocking: got %q", got.ExternalID)
	}

	// Verify the STORE has the ExternalID — that's what survives a
	// daemon restart and what the TUI needs to resume the session.
	persisted, err := d.Store.LoadSessions()
	if err != nil {
		t.Fatalf("Store.LoadSessions: %v", err)
	}
	var found *agent.SessionInfo
	for i := range persisted {
		if persisted[i].ID == info.ID {
			found = &persisted[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("persisted session %s not found in store", info.ID)
	}
	if found.ExternalID != "real-backend-session-id-123" {
		t.Fatalf("persisted ExternalID = %q, want %q — a daemon restart at this point would lose it forever (production bug)", found.ExternalID, "real-backend-session-id-123")
	}
}
