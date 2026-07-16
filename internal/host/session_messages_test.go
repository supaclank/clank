package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hoststore "github.com/acksell/clank/internal/host/store"
)

// tripwireClaudeManager wraps the real ClaudeBackendManager (so the
// TranscriptReader capability and transcript decoding are the production
// code paths) but trips on CreateBackend — a pure history read must never
// build a backend, let alone spawn the claude CLI.
type tripwireClaudeManager struct {
	*host.ClaudeBackendManager
	mu          sync.Mutex
	createCalls int
}

func (m *tripwireClaudeManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	m.mu.Lock()
	m.createCalls++
	m.mu.Unlock()
	return nil, errors.New("unexpected CreateBackend on the messages read path")
}

func (m *tripwireClaudeManager) createCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createCalls
}

func newStoreForTest(t *testing.T) *hoststore.Store {
	t.Helper()
	st, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestSessionMessages_ClaudeColdReadDoesNotSpawnBackend is the regression
// test for the 25s cold session-open (clank#158): GET /sessions/{id}/messages
// used to run ensureBackend, whose unconditional Open() spawned the claude
// CLI (measured 24.7s on a cold Fly rootfs) and registered a backend whose
// constructor status rendered as a stuck "Starting…" — even though claude
// history is a plain JSONL read. A messages fetch for a claude session with
// no live backend must serve the on-disk transcript and leave the live
// registry untouched.
func TestSessionMessages_ClaudeColdReadDoesNotSpawnBackend(t *testing.T) {
	// Cannot use t.Parallel because t.Setenv mutates process env.
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	repo := initGitRepo(t, "git@example.com:acme/repo.git")
	projDir := mkClaudeProjectDirForHost(t, configDir, repo)

	const extID = "sess-cold-read-001"
	writeSessionJSONLForHost(t, projDir, extID, []map[string]any{
		{
			"type":      "user",
			"uuid":      "u-1",
			"timestamp": "2026-07-16T10:00:01Z",
			"sessionId": extID,
			"cwd":       repo,
			"message":   map[string]any{"role": "user", "content": "Hello from disk"},
		},
		{
			"type":      "assistant",
			"uuid":      "a-1",
			"timestamp": "2026-07-16T10:00:02Z",
			"sessionId": extID,
			"message": map[string]any{
				"id":      "msg_cold_001",
				"model":   "claude-sonnet-4",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": "Hi from history"}},
			},
		},
	})

	st := newStoreForTest(t)
	mgr := &tripwireClaudeManager{ClaudeBackendManager: host.NewClaudeBackendManager()}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendClaudeCode: mgr,
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	const id = "01COLDREAD0000000000000001"
	if err := st.UpsertSession(ctx, agent.SessionInfo{
		ID:         id,
		ExternalID: extID,
		Backend:    agent.BackendClaudeCode,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{LocalPath: repo},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	msgs, err := svc.SessionMessages(ctx, id)
	if err != nil {
		t.Fatalf("SessionMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "Hello from disk" {
		t.Errorf("msgs[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || len(msgs[1].Parts) != 1 || msgs[1].Parts[0].Text != "Hi from history" {
		t.Errorf("msgs[1] = %+v", msgs[1])
	}

	if got := mgr.createCount(); got != 0 {
		t.Errorf("CreateBackend calls = %d, want 0 (transcript read must not build a backend)", got)
	}
	if _, ok := svc.Session(id); ok {
		t.Error("a backend was registered by a messages read; the read path must not mutate the live registry")
	}
}

// TestSessionMessages_ClaudeFreshSessionHasNoTranscript pins the empty-
// ExternalID contract: a claude session that was created but never opened
// has no transcript — the read returns empty history, not an error, and
// still builds no backend.
func TestSessionMessages_ClaudeFreshSessionHasNoTranscript(t *testing.T) {
	t.Parallel()

	repo := initGitRepo(t, "git@example.com:acme/repo.git")
	st := newStoreForTest(t)
	mgr := &tripwireClaudeManager{ClaudeBackendManager: host.NewClaudeBackendManager()}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendClaudeCode: mgr,
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	const id = "01FRESHNOEXT00000000000001"
	if err := st.UpsertSession(ctx, agent.SessionInfo{
		ID:      id,
		Backend: agent.BackendClaudeCode,
		Status:  agent.StatusIdle,
		GitRef:  agent.GitRef{LocalPath: repo},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	msgs, err := svc.SessionMessages(ctx, id)
	if err != nil {
		t.Fatalf("SessionMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("got %d messages, want 0 for a never-opened session", len(msgs))
	}
	if got := mgr.createCount(); got != 0 {
		t.Errorf("CreateBackend calls = %d, want 0", got)
	}
}

// transcriptAwareManager is a fixture manager that implements both
// agent.BackendManager and agent.TranscriptReader with distinguishable
// outputs, so tests can tell which path served a messages read.
type transcriptAwareManager struct {
	liveMsgs       []agent.MessageData
	transcriptMsgs []agent.MessageData

	mu          sync.Mutex
	createCalls int
	readCalls   int
}

func (m *transcriptAwareManager) Init(_ context.Context, _ func() ([]string, error)) error {
	return nil
}

func (m *transcriptAwareManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	m.mu.Lock()
	m.createCalls++
	m.mu.Unlock()
	return &messagesBackend{msgs: m.liveMsgs}, nil
}

func (m *transcriptAwareManager) Shutdown() {}

func (m *transcriptAwareManager) ReadTranscript(_ context.Context, _, _ string) ([]agent.MessageData, error) {
	m.mu.Lock()
	m.readCalls++
	m.mu.Unlock()
	return m.transcriptMsgs, nil
}

func (m *transcriptAwareManager) counts() (creates, reads int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createCalls, m.readCalls
}

// messagesBackend is a noop backend whose Messages returns fixed data and
// whose status can be flipped to dead.
type messagesBackend struct {
	msgs []agent.MessageData

	mu   sync.Mutex
	dead bool
}

func (b *messagesBackend) setDead() {
	b.mu.Lock()
	b.dead = true
	b.mu.Unlock()
}

func (b *messagesBackend) Open(_ context.Context) error { return nil }
func (b *messagesBackend) OpenAndSend(_ context.Context, _ agent.SendMessageOpts) error {
	return nil
}
func (b *messagesBackend) Send(_ context.Context, _ agent.SendMessageOpts) error { return nil }
func (b *messagesBackend) Abort(_ context.Context) error                         { return nil }
func (b *messagesBackend) Stop() error                                           { return nil }
func (b *messagesBackend) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (b *messagesBackend) Status() agent.SessionStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dead {
		return agent.StatusDead
	}
	return agent.StatusIdle
}
func (b *messagesBackend) SessionID() string { return "ext-live" }
func (b *messagesBackend) Messages(_ context.Context) ([]agent.MessageData, error) {
	return b.msgs, nil
}
func (b *messagesBackend) Revert(_ context.Context, _ string) error { return nil }
func (b *messagesBackend) Fork(_ context.Context, _ string) (agent.ForkResult, error) {
	return agent.ForkResult{}, nil
}
func (b *messagesBackend) RespondPermission(_ context.Context, _ string, _ bool, _ string) error {
	return nil
}
func (b *messagesBackend) RespondQuestion(_ context.Context, _ string, _ []agent.QuestionAnswer, _ bool) error {
	return nil
}

// TestSessionMessages_LiveBackendWinsOverTranscript asserts precedence: a
// LIVE registered backend keeps serving Messages (streaming turns include
// the in-memory revert filter etc.), even when the manager could read the
// transcript from disk.
func TestSessionMessages_LiveBackendWinsOverTranscript(t *testing.T) {
	t.Parallel()

	repo := initGitRepo(t, "git@example.com:acme/repo.git")
	st := newStoreForTest(t)
	mgr := &transcriptAwareManager{
		liveMsgs:       []agent.MessageData{{Role: "assistant", Content: "from live backend"}},
		transcriptMsgs: []agent.MessageData{{Role: "assistant", Content: "from transcript"}},
	}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendClaudeCode: mgr,
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	const id = "01LIVEWINS0000000000000001"
	if err := st.UpsertSession(ctx, agent.SessionInfo{
		ID:         id,
		ExternalID: "sess-live-001",
		Backend:    agent.BackendClaudeCode,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{LocalPath: repo},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Register a live backend the way a real dispatch does.
	if _, _, err := svc.OpenSession(ctx, id); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if _, ok := svc.Session(id); !ok {
		t.Fatal("expected a live backend after OpenSession")
	}

	msgs, err := svc.SessionMessages(ctx, id)
	if err != nil {
		t.Fatalf("SessionMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "from live backend" {
		t.Errorf("msgs = %+v, want the live backend's data", msgs)
	}
	creates, reads := mgr.counts()
	if creates != 1 {
		t.Errorf("CreateBackend calls = %d, want 1 (only the OpenSession)", creates)
	}
	if reads != 0 {
		t.Errorf("ReadTranscript calls = %d, want 0 while a live backend exists", reads)
	}
}

// TestSessionMessages_DeadBackendServedFromTranscript pins the read-path
// half of [INV-DEAD-BACKEND-REHYDRATE-001]: a read does NOT repair a dead
// backend — it serves the transcript and leaves rehydration to the next
// dispatching op — so history stays available without a CLI respawn.
func TestSessionMessages_DeadBackendServedFromTranscript(t *testing.T) {
	t.Parallel()

	repo := initGitRepo(t, "git@example.com:acme/repo.git")
	st := newStoreForTest(t)
	mgr := &transcriptAwareManager{
		liveMsgs:       []agent.MessageData{{Role: "assistant", Content: "from live backend"}},
		transcriptMsgs: []agent.MessageData{{Role: "assistant", Content: "from transcript"}},
	}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendClaudeCode: mgr,
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	const id = "01DEADREAD0000000000000001"
	if err := st.UpsertSession(ctx, agent.SessionInfo{
		ID:         id,
		ExternalID: "sess-dead-001",
		Backend:    agent.BackendClaudeCode,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{LocalPath: repo},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, err := svc.OpenSession(ctx, id); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	b, ok := svc.Session(id)
	if !ok {
		t.Fatal("expected a live backend after OpenSession")
	}
	b.(*messagesBackend).setDead()

	msgs, err := svc.SessionMessages(ctx, id)
	if err != nil {
		t.Fatalf("SessionMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "from transcript" {
		t.Errorf("msgs = %+v, want the transcript's data", msgs)
	}
	creates, reads := mgr.counts()
	if creates != 1 {
		t.Errorf("CreateBackend calls = %d, want 1 (the read must not rehydrate)", creates)
	}
	if reads != 1 {
		t.Errorf("ReadTranscript calls = %d, want 1", reads)
	}
}

// openCountingManager is a fixture for backends without a TranscriptReader
// (opencode): CreateBackend counts and hands out live backends.
type openCountingManager struct {
	mu          sync.Mutex
	createCalls int
}

func (m *openCountingManager) Init(_ context.Context, _ func() ([]string, error)) error { return nil }

func (m *openCountingManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	m.mu.Lock()
	m.createCalls++
	m.mu.Unlock()
	return &messagesBackend{msgs: []agent.MessageData{{Role: "assistant", Content: "from rehydrated backend"}}}, nil
}

func (m *openCountingManager) Shutdown() {}

func (m *openCountingManager) createCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createCalls
}

// TestSessionMessages_OpenCodeRehydratesViaEnsureBackend pins that backends
// whose history API needs a live server (opencode has no TranscriptReader)
// keep the ensureBackend behavior on a cold messages read: create, open,
// register, then serve Messages through the backend.
func TestSessionMessages_OpenCodeRehydratesViaEnsureBackend(t *testing.T) {
	t.Parallel()

	repo := initGitRepo(t, "git@example.com:acme/repo.git")
	st := newStoreForTest(t)
	mgr := &openCountingManager{}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: mgr,
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	const id = "01OCREHYDRATE0000000000001"
	if err := st.UpsertSession(ctx, agent.SessionInfo{
		ID:         id,
		ExternalID: "ses_opencode_001",
		Backend:    agent.BackendOpenCode,
		Status:     agent.StatusIdle,
		GitRef:     agent.GitRef{LocalPath: repo},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	msgs, err := svc.SessionMessages(ctx, id)
	if err != nil {
		t.Fatalf("SessionMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "from rehydrated backend" {
		t.Errorf("msgs = %+v, want the rehydrated backend's data", msgs)
	}
	if got := mgr.createCount(); got != 1 {
		t.Errorf("CreateBackend calls = %d, want 1 (opencode must rehydrate on read)", got)
	}
	if _, ok := svc.Session(id); !ok {
		t.Error("expected the rehydrated backend to be registered")
	}
}

// --- Claude transcript fixture helpers (mirror the SDK's on-disk layout;
// same scheme as internal/agent/claude_messages_test.go) ---

// mkClaudeProjectDirForHost creates the per-cwd project directory inside a
// CLAUDE_CONFIG_DIR-pointed config dir, mirroring the SDK's encodeCwd
// (replace every non-alphanumeric rune with "-" after Abs).
func mkClaudeProjectDirForHost(t *testing.T, configDir, cwd string) string {
	t.Helper()
	abs, err := filepath.Abs(cwd)
	if err != nil {
		t.Fatalf("Abs(%q): %v", cwd, err)
	}
	var b strings.Builder
	b.Grow(len(abs))
	for _, r := range abs {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	dir := filepath.Join(configDir, "projects", b.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

// writeSessionJSONLForHost writes one JSON object per line to
// <dir>/<sessionID>.jsonl.
func writeSessionJSONLForHost(t *testing.T, dir, sessionID string, entries []map[string]any) {
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
