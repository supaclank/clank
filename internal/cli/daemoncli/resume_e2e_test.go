package daemoncli

// Regression coverage for the "session not found after restart" bug.
//
// Symptom in the wild: a session created in run #1 of clankd is
// listed by GET /sessions in run #2, but clicking to open it errors
// with `session 01K…: host: not found`. Root cause: the live-registry
// lookup in handleSendSession / handleGetMessages / etc 404s when the
// store has the row but no live backend is registered yet. The
// session has to be lazily rehydrated from store on first access.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestResume_SendToPersistedSessionRehydrates simulates the daemon
// restart by stuffing a row into the host store BEFORE any session is
// live in memory, then sending a follow-up message. The host must
// recognize the persisted row, rehydrate the backend, and accept the
// send. Without ResumeSession this 404s with "session: host: not found".
func TestResume_SendToPersistedSessionRehydrates(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)

	// Pre-seed the store with a session that has no live backend.
	// Mirrors what the daemon sees on startup after the previous
	// run's CreateSession persisted this row and the daemon was killed.
	repo := initTestGitRepo(t)
	const id = "01PERSISTEDSESSION0000"
	persisted := agent.SessionInfo{
		ID:         id,
		ExternalID: "ext-from-previous-run",
		Backend:    agent.BackendOpenCode,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{LocalPath: repo, RemoteURL: "git@example.com:acme/repo.git"},
		Prompt:     "first turn from yesterday",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := td.Store.UpsertSession(ctx, persisted); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Now act as the TUI: send a follow-up. Pre-fix this 404'd; the
	// fix lazily rehydrates the backend through ResumeSession.
	if err := td.Client.Session(id).Send(ctx, agent.SendMessageOpts{Text: "follow-up after restart"}); err != nil {
		t.Fatalf("Session(%s).Send: %v (regression: persisted session was not rehydrated)", id, err)
	}

	// And the rehydrated backend should now be in the live registry.
	if _, ok := td.Service.Session(id); !ok {
		t.Error("backend not registered after rehydrate")
	}
}

// TestResume_GetMessagesRehydrates pins the same contract for the
// other half of the TUI's open-session flow: fetchSessionMessages
// hits GET /sessions/{id}/messages, which used to 404 the same way.
func TestResume_GetMessagesRehydrates(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	repo := initTestGitRepo(t)
	const id = "01MESSAGESPERSIST00000"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := td.Store.UpsertSession(ctx, agent.SessionInfo{
		ID:         id,
		ExternalID: "ext-prev",
		Backend:    agent.BackendOpenCode,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{LocalPath: repo, RemoteURL: "git@example.com:acme/repo.git"},
		Prompt:     "old",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := td.Client.Session(id).Messages(ctx); err != nil {
		t.Fatalf("Session(%s).Messages: %v", id, err)
	}
}

// TestResume_MarkReadOnPersistedSession verifies bug #3 (mark-read on
// a persisted-but-not-live session). MarkRead writes through the
// store, so this never needed the live backend; the test pins that
// the store-only path doesn't accidentally regress to require one.
func TestResume_MarkReadOnPersistedSession(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	repo := initTestGitRepo(t)
	const id = "01MARKREADPERSIST00000"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := td.Store.UpsertSession(ctx, agent.SessionInfo{
		ID:      id,
		Backend: agent.BackendOpenCode,
		Status:  agent.StatusIdle,
		GitRef:  agent.GitRef{LocalPath: repo, RemoteURL: "git@example.com:acme/repo.git"},
		Prompt:  "old",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := td.Client.Session(id).MarkRead(ctx); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	got, err := td.Store.GetSession(ctx, id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.LastReadAt.IsZero() {
		t.Error("LastReadAt is still zero after MarkRead")
	}
}

// TestResume_DoesNotRespawnIfAlreadyLive pins the cache hit: when the
// session IS in the live registry already, ResumeSession returns the
// existing backend rather than spawning a duplicate.
func TestResume_DoesNotRespawnIfAlreadyLive(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	info, b := td.CreateOpenCodeSession(t, "tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Send drives ResumeSession internally. The stub backend records
	// every CreateBackend call via stubBackendManager.last; if a
	// rehydrate spawned a duplicate, last would point at a different
	// instance after the call.
	if err := td.Client.Session(info.ID).Send(ctx, agent.SendMessageOpts{Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	td.Backend.mu.Lock()
	last := td.Backend.last
	td.Backend.mu.Unlock()
	if last != b {
		t.Error("ResumeSession spawned a duplicate backend instead of reusing the live one")
	}
}

// TestResume_NotFoundForUnknownSession pins the negative case: a
// session id that's neither live nor persisted still returns
// not-found, not silently auto-creates.
func TestResume_NotFoundForUnknownSession(t *testing.T) {
	t.Parallel()
	td := newTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := td.Client.Session("01DEFINITELYNOTFOUND00").Send(ctx, agent.SendMessageOpts{Text: "x"})
	if err == nil {
		t.Fatal("expected not-found, got nil")
	}
}

// TestResume_SurvivesAcrossNewStoreOpen mirrors a real daemon restart:
// daemon #1 writes the row and tears down (host service Shutdown +
// store Close). Daemon #2 opens a fresh store at the same path and
// must find the row to lazily rehydrate the backend on Send.
//
// Without close/reopen between daemons, the test would only exercise
// the live-store path which TestResume_SendToPersistedSessionRehydrates
// already covers. The restart shape is what catches a regression where
// migrations or write-buffering would leave the row unreadable to a
// fresh connection.
func TestResume_SurvivesAcrossNewStoreOpen(t *testing.T) {
	t.Parallel()
	repo := initTestGitRepo(t)
	const id = "01ACROSSREOPENABCDEF00"

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "host.db")

	// Daemon #1: seed the row.
	td1 := newTestDaemonAt(t, dbPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := td1.Store.UpsertSession(ctx, agent.SessionInfo{
		ID:      id,
		Backend: agent.BackendOpenCode,
		Status:  agent.StatusIdle,
		GitRef:  agent.GitRef{LocalPath: repo, RemoteURL: "git@example.com:acme/repo.git"},
		Prompt:  "from before",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	td1.Service.Shutdown()
	if err := td1.Store.Close(); err != nil {
		t.Fatalf("close store on daemon #1: %v", err)
	}

	// Daemon #2 on the same dbPath — fresh connection, fresh service.
	td2 := newTestDaemonAt(t, dbPath)
	if err := td2.Client.Session(id).Send(ctx, agent.SendMessageOpts{Text: "after restart"}); err != nil {
		t.Fatalf("Send through daemon #2: %v (regression: row not visible across reopen)", err)
	}
}
