package hub_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostclient "github.com/acksell/clank/internal/host/client"
	hostmux "github.com/acksell/clank/internal/host/mux"
	"github.com/acksell/clank/internal/hub"
	hubclient "github.com/acksell/clank/internal/hub/client"
	hubmux "github.com/acksell/clank/internal/hub/mux"
	"github.com/acksell/clank/internal/store"
)

// TestPersistence_HealsMistaggedClaudeSessionAcrossRestart reproduces the
// production bug: an old daemon (pre-fix) persisted a real Claude Code
// session with Backend=opencode (the loop hardcoded the tag for every
// snapshot it discovered). After clankd restart the corrupted row routes
// SessionMessages through the OpenCode backend manager, which has no idea
// about the session, and the TUI hangs on "Waiting for agent output...".
//
// The test exercises the full post-restart code path:
//
//  1. Seed a real Claude Code JSONL transcript on disk under a temp
//     CLAUDE_CONFIG_DIR.
//  2. Open a SQLite store and directly UpsertSession a corrupted row
//     (Backend=opencode, ExternalID=<the Claude session UUID>) — bypassing
//     Discover so the bad state is exactly what a pre-fix daemon would
//     have written.
//  3. Boot a fresh hub.Service wired with the real ClaudeBackendManager
//     and a benign mock for OpenCode (so a mis-routed call would surface
//     as a deterministic failure rather than a hang).
//  4. WaitStartupDiscover for the heal pass to finish.
//  5. Assert the persisted Backend is now claude-code and that
//     SessionMessages returns the JSONL transcript content (proving the
//     dispatch was routed to Claude post-heal, not to the OpenCode mock).
//
// Cannot t.Parallel because t.Setenv mutates process env.
func TestPersistence_HealsMistaggedClaudeSessionAcrossRestart(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	workDir := t.TempDir()
	// The host's workdir resolver requires LocalPath to be a real git repo.
	initGitRepoAt(t, workDir, "git@github.com:acksell/claude-sessions-test.git")
	projDir := mkClaudeProjectDirForHub(t, configDir, workDir)

	const claudeSessionID = "claude-sess-corrupt-001"
	writeClaudeJSONLForHub(t, projDir, claudeSessionID, []map[string]any{
		{
			"type":      "user",
			"uuid":      "u-1",
			"timestamp": "2026-04-25T10:00:01Z",
			"sessionId": claudeSessionID,
			"cwd":       workDir,
			"message":   map[string]any{"role": "user", "content": "what's up?"},
		},
		{
			"type":      "assistant",
			"uuid":      "a-1",
			"timestamp": "2026-04-25T10:00:02Z",
			"sessionId": claudeSessionID,
			"cwd":       workDir,
			"message": map[string]any{
				"id":      "msg_001",
				"type":    "message",
				"role":    "assistant",
				"model":   "claude-sonnet",
				"content": []map[string]any{{"type": "text", "text": "all good"}},
			},
		},
	})

	dir := shortTempDir(t)
	dbPath := filepath.Join(dir, "test.db")
	sockPath := filepath.Join(dir, "test.sock")

	// --- Phase 1: seed the corrupted row directly. ---
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	const hubID = "hub-corrupt-001"
	corrupted := agent.SessionInfo{
		ID:         hubID,
		ExternalID: claudeSessionID,
		Backend:    agent.BackendOpenCode, // <-- the bug: tagged as opencode
		Status:     agent.StatusIdle,
		Hostname:   "local",
		GitRef:     agent.GitRef{LocalPath: workDir},
		Title:      "old corrupted session",
		Prompt:     "what's up?",
		CreatedAt:  time.Now().Add(-1 * time.Hour),
		UpdatedAt:  time.Now().Add(-30 * time.Minute),
	}
	if err := st1.UpsertSession(corrupted); err != nil {
		t.Fatalf("UpsertSession (seed): %v", err)
	}
	st1.Close()

	// --- Phase 2: restart with the real Claude manager wired in. ---
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open (phase 2): %v", err)
	}

	s := hub.New()
	s.Store = st2
	// Real Claude manager — this is what the heal must route to.
	s.BackendManagers[agent.BackendClaudeCode] = host.NewClaudeBackendManager()
	// Benign mock for OpenCode. If the bug regresses and SessionMessages
	// gets dispatched here, the mock will return a no-history backend
	// rather than hang, and the assertion below will fail clearly.
	s.BackendManagers[agent.BackendOpenCode] = newMockBackendManager()

	client, cleanup := startClaudeHostHubAtSocket(t, s, sockPath)
	defer func() {
		cleanup()
		st2.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.WaitStartupDiscover(ctx); err != nil {
		t.Fatalf("WaitStartupDiscover: %v", err)
	}

	// The heal must have rewritten Backend → claude-code.
	sessions, err := client.Sessions().List(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session after restart, got %d", len(sessions))
	}
	if sessions[0].Backend != agent.BackendClaudeCode {
		t.Fatalf("Backend = %q, want %q (startup-discover should have healed the mis-tagged row)", sessions[0].Backend, agent.BackendClaudeCode)
	}

	// And SessionMessages must return the JSONL transcript (the real proof
	// that the dispatch is routed through Claude — the mock OpenCode backend
	// would return nothing).
	msgs, err := client.Session(hubID).Messages(ctx)
	if err != nil {
		t.Fatalf("SessionMessages: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("got %d messages, want >= 2 (user + assistant from JSONL)", len(msgs))
	}
}

// startClaudeHostHubAtSocket is a streamlined variant of startHubAtSocket
// used by TestPersistence_HealsMistaggedClaudeSessionAcrossRestart. It
// builds the host fixture from s.BackendManagers (so the real Claude
// manager is reachable through the host wire path) and binds the hub to
// the supplied socket so the test controls the listener lifetime
// directly. We avoid the standard helper because we don't want the
// implicit registerTestRepo seeding (this test uses a Local GitRef, not
// the canonical testRemoteURL).
func startClaudeHostHubAtSocket(t *testing.T, s *hub.Service, sockPath string) (*hubclient.Client, func()) {
	t.Helper()

	clonesDir := t.TempDir()
	hostSvc := host.New(host.Options{BackendManagers: s.BackendManagers, ClonesDir: clonesDir})
	if err := hostSvc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("host.Init: %v", err)
	}
	hostSrv := httptest.NewServer(hostmux.New(hostSvc, nil).Handler())
	hc := hostclient.NewHTTP(hostSrv.URL, nil)
	s.SetHostClient(hc)

	os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ln, hubmux.New(s, nil).Handler()) }()

	client := hubclient.NewClient(sockPath)
	waitForDaemon(t, client)

	cleanup := func() {
		s.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
		_ = hc.Close()
		hostSrv.Close()
		hostSvc.Shutdown()
		os.Remove(sockPath)
	}
	return client, cleanup
}

// --- Local helpers (mirror those in internal/host/backends_test.go and
// internal/agent/claude_messages_test.go; duplicated to keep the test
// self-contained without exporting test-only fixture utilities). ---

func mkClaudeProjectDirForHub(t *testing.T, configDir, cwd string) string {
	t.Helper()
	abs, err := filepath.Abs(cwd)
	if err != nil {
		t.Fatalf("Abs(%q): %v", cwd, err)
	}
	encoded := encodeCwdLikeSDKForHub(abs)
	dir := filepath.Join(configDir, "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

func encodeCwdLikeSDKForHub(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func writeClaudeJSONLForHub(t *testing.T, dir, sessionID string, entries []map[string]any) {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode entry: %v", err)
		}
	}
}
