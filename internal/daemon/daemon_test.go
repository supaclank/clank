package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemon"
	"github.com/acksell/clank/internal/store"
)

// mockBackend implements agent.SessionBackend for testing.
type mockBackend struct {
	mu                sync.Mutex
	events            chan agent.Event
	status            agent.SessionStatus
	sessionID         string
	started           bool
	stopped           bool
	messages          []string
	aborted           bool
	reverted          bool
	revertedMessageID string

	// history is returned by Messages(). Tests can set it to control
	// the message history returned by the backend.
	history []agent.MessageData

	// onStart is called during Start, allowing tests to control behavior.
	onStart func(ctx context.Context, req agent.StartRequest) error
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		events:    make(chan agent.Event, 64),
		status:    agent.StatusStarting,
		sessionID: "mock-session-1",
	}
}

func (m *mockBackend) Start(ctx context.Context, req agent.StartRequest) error {
	m.mu.Lock()
	m.started = true
	m.status = agent.StatusBusy
	m.mu.Unlock()

	// Emit a status change event.
	m.events <- agent.Event{
		Type:      agent.EventStatusChange,
		Timestamp: time.Now(),
		Data: agent.StatusChangeData{
			OldStatus: agent.StatusStarting,
			NewStatus: agent.StatusBusy,
		},
	}

	if m.onStart != nil {
		return m.onStart(ctx, req)
	}
	return nil
}

func (m *mockBackend) Watch(ctx context.Context) error {
	return nil
}

func (m *mockBackend) SendMessage(ctx context.Context, opts agent.SendMessageOpts) error {
	m.mu.Lock()
	m.messages = append(m.messages, opts.Text)
	m.mu.Unlock()

	// Emit a message event.
	m.events <- agent.Event{
		Type:      agent.EventMessage,
		Timestamp: time.Now(),
		Data: agent.MessageData{
			Role:    "user",
			Content: opts.Text,
		},
	}
	return nil
}

func (m *mockBackend) Abort(ctx context.Context) error {
	m.mu.Lock()
	m.aborted = true
	m.status = agent.StatusIdle
	m.mu.Unlock()

	m.events <- agent.Event{
		Type:      agent.EventStatusChange,
		Timestamp: time.Now(),
		Data: agent.StatusChangeData{
			OldStatus: agent.StatusBusy,
			NewStatus: agent.StatusIdle,
		},
	}
	return nil
}

func (m *mockBackend) Stop() error {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	close(m.events)
	return nil
}

func (m *mockBackend) Events() <-chan agent.Event {
	return m.events
}

func (m *mockBackend) Status() agent.SessionStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *mockBackend) SessionID() string {
	return m.sessionID
}

func (m *mockBackend) Messages(ctx context.Context) ([]agent.MessageData, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.history, nil
}

func (m *mockBackend) Revert(ctx context.Context, messageID string) error {
	m.mu.Lock()
	m.reverted = true
	m.revertedMessageID = messageID
	m.mu.Unlock()
	return nil
}

// mockBackendManager implements agent.BackendManager for testing. It wraps
// a creation function so tests can intercept and track created backends.
type mockBackendManager struct {
	mu      sync.Mutex
	create  func(req agent.StartRequest) *mockBackend // custom creation logic; nil = newMockBackend()
	latest  *mockBackend                              // last created backend
	all     []*mockBackend                            // all created backends
	stopped bool
}

func newMockBackendManager() *mockBackendManager {
	return &mockBackendManager{}
}

func (m *mockBackendManager) CreateBackend(req agent.StartRequest) (agent.SessionBackend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b *mockBackend
	if m.create != nil {
		b = m.create(req)
	} else {
		b = newMockBackend()
	}
	m.latest = b
	m.all = append(m.all, b)
	return b, nil
}

func (m *mockBackendManager) Init(ctx context.Context, knownDirs func() ([]string, error)) error {
	return nil
}

func (m *mockBackendManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
}

// getLatest returns the most recently created mockBackend.
func (m *mockBackendManager) getLatest() *mockBackend {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.latest
}

// getAll returns all created mockBackends.
func (m *mockBackendManager) getAll() []*mockBackend {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]*mockBackend, len(m.all))
	copy(cp, m.all)
	return cp
}

// mockDiscovererManager implements BackendManager + SessionDiscoverer.
type mockDiscovererManager struct {
	mockBackendManager
	snapshots []agent.SessionSnapshot
}

func (m *mockDiscovererManager) DiscoverSessions(ctx context.Context, seedDir string) ([]agent.SessionSnapshot, error) {
	return m.snapshots, nil
}

// mockAgentListerManager implements BackendManager + AgentLister.
type mockAgentListerManager struct {
	mockBackendManager
	agents func(ctx context.Context, projectDir string) ([]agent.AgentInfo, error)
}

func (m *mockAgentListerManager) ListAgents(ctx context.Context, projectDir string) ([]agent.AgentInfo, error) {
	return m.agents(ctx, projectDir)
}

// shortTempDir creates a temp directory with a short path suitable for Unix sockets.
// macOS has a 104-char limit on socket paths, and t.TempDir() can produce
// paths that exceed this when combined with long test names.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "clank-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// testDaemon creates a daemon in a temp directory, starts it, and returns
// the daemon, a connected client, and a cleanup function.
func testDaemon(t *testing.T) (*daemon.Daemon, *daemon.Client, func()) {
	t.Helper()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)

	// Wire up mock backend manager for all backend types.
	mgr := newMockBackendManager()
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr

	// Start daemon in background.
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run()
	}()

	// Wait for socket to exist.
	client := daemon.NewClient(sockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if err := client.Ping(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("daemon did not start in time")
		case err := <-errCh:
			t.Fatalf("daemon exited unexpectedly: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}

	cleanup := func() {
		d.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
	}

	// Store lastBackend accessor on test via a helper.
	// We'll access it through the test closure.
	_ = mgr

	return d, client, cleanup
}

// --- Tests ---

func TestDaemonPing(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	resp, err := client.PingInfo(ctx)
	if err != nil {
		t.Fatalf("PingInfo: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status=ok, got %s", resp.Status)
	}
	if resp.PID == 0 {
		t.Error("expected non-zero PID")
	}
	if resp.Version == "" {
		t.Error("expected non-empty version")
	}
}

func TestDaemonPIDFile(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	// Wait for PID file.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client := daemon.NewClient(sockPath)
	for {
		if err := client.Ping(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("daemon did not start in time")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// PID file should exist.
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read PID file: %v", err)
	}
	if len(data) == 0 {
		t.Error("PID file is empty")
	}

	// Stop daemon.
	d.Stop()
	<-errCh

	// PID file should be cleaned up.
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("PID file should be removed after stop")
	}
	// Socket should be cleaned up.
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("socket file should be removed after stop")
	}
}

func TestDaemonCreateSession(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test-project",
		Prompt:     "Fix the bug",
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
	if info.ProjectDir != "/tmp/test-project" {
		t.Errorf("expected project_dir=/tmp/test-project, got %s", info.ProjectDir)
	}
	if info.Prompt != "Fix the bug" {
		t.Errorf("expected prompt='Fix the bug', got %s", info.Prompt)
	}
	if info.ProjectName != "test-project" {
		t.Errorf("expected project_name=test-project, got %s", info.ProjectName)
	}
}

func TestDaemonCreateSessionValidation(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()

	// Missing backend.
	_, err := client.CreateSession(ctx, agent.StartRequest{
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err == nil {
		t.Error("expected error for missing backend")
	}

	// Missing project dir.
	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		Prompt:  "test",
	})
	if err == nil {
		t.Error("expected error for missing project_dir")
	}

	// Missing prompt.
	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
	})
	if err == nil {
		t.Error("expected error for missing prompt")
	}

	// Invalid backend.
	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    "invalid",
		ProjectDir: "/tmp/test",
		Prompt:     "test",
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
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}

	// Create two sessions.
	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/project-a",
		Prompt:     "task a",
	})
	if err != nil {
		t.Fatalf("CreateSession a: %v", err)
	}

	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/project-b",
		Prompt:     "task b",
	})
	if err != nil {
		t.Fatalf("CreateSession b: %v", err)
	}

	// Allow time for backends to start.
	time.Sleep(100 * time.Millisecond)

	sessions, err = client.ListSessions(ctx)
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := client.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ID != info.ID {
		t.Errorf("expected ID=%s, got %s", info.ID, got.ID)
	}

	// Non-existent session.
	_, err = client.GetSession(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent session")
	}
}

func TestDaemonGetSessionMessages(t *testing.T) {
	t.Parallel()
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
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

	messages, err := client.GetSessionMessages(ctx, info.ID)
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Backend returns nil history by default — should get empty array.
	messages, err := client.GetSessionMessages(ctx, info.ID)
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

	_, err := client.GetSessionMessages(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent session")
	}
}

func TestDaemonSendMessage(t *testing.T) {
	mgr := newMockBackendManager()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer func() {
		d.Stop()
		<-errCh
	}()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "initial prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Wait for backend to start.
	time.Sleep(100 * time.Millisecond)

	err = client.SendMessage(ctx, info.ID, agent.SendMessageOpts{Text: "follow-up message"})
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

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer func() {
		d.Stop()
		<-errCh
	}()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "do stuff",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	err = client.AbortSession(ctx, info.ID)
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

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer func() {
		d.Stop()
		<-errCh
	}()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "do stuff",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	err = client.RevertSession(ctx, info.ID, "msg-abc-123")
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Empty message_id should return an error.
	err = client.RevertSession(ctx, info.ID, "")
	if err == nil {
		t.Error("expected error for empty message_id")
	}
}

func TestDaemonRevertSessionNotFound(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	err := client.RevertSession(context.Background(), "nonexistent", "msg-123")
	if err == nil {
		t.Error("expected error reverting non-existent session")
	}
}

func TestDaemonDeleteSession(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	err = client.DeleteSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// Session should be gone.
	_, err = client.GetSession(ctx, info.ID)
	if err == nil {
		t.Error("expected error getting deleted session")
	}

	sessions, err := client.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions after delete, got %d", len(sessions))
	}
}

func TestDaemonEventStream(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Subscribe to events.
	events, err := client.SubscribeEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	// Create a session — should generate events.
	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// We should receive a session.create event and a status change event.
	received := make([]agent.Event, 0)
	timeout := time.After(2 * time.Second)

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Fatal("event channel closed unexpectedly")
			}
			received = append(received, evt)
			if len(received) >= 2 {
				goto done
			}
		case <-timeout:
			goto done
		}
	}
done:

	if len(received) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(received))
	}

	// First event should be session.create.
	if received[0].Type != agent.EventSessionCreate {
		t.Errorf("expected first event type=session.create, got %s", received[0].Type)
	}

	// Second should be status change (starting -> busy).
	if received[1].Type != agent.EventStatusChange {
		t.Errorf("expected second event type=status, got %s", received[1].Type)
	}
}

func TestDaemonStatus(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()

	// Create a session.
	_, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	status, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if status.PID == 0 {
		t.Error("expected non-zero PID")
	}
	if len(status.Sessions) != 1 {
		t.Errorf("expected 1 session in status, got %d", len(status.Sessions))
	}
}

func TestDaemonGracefulShutdownStopsBackends(t *testing.T) {
	mgr := newMockBackendManager()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)

	ctx := context.Background()

	// Create two sessions.
	_, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/a",
		Prompt:     "task a",
	})
	if err != nil {
		t.Fatalf("CreateSession a: %v", err)
	}
	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/b",
		Prompt:     "task b",
	})
	if err != nil {
		t.Fatalf("CreateSession b: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Stop daemon.
	d.Stop()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop in time")
	}

	// All backends should have been stopped.
	backends := mgr.getAll()
	for i, b := range backends {
		b.mu.Lock()
		if !b.stopped {
			t.Errorf("backend %d was not stopped during shutdown", i)
		}
		b.mu.Unlock()
	}
}

func TestDaemonSendMessageToNonexistentSession(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	err := client.SendMessage(ctx, "nonexistent", agent.SendMessageOpts{Text: "hello"})
	if err == nil {
		t.Error("expected error sending to non-existent session")
	}
}

func TestDaemonSendEmptyMessage(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	err = client.SendMessage(ctx, info.ID, agent.SendMessageOpts{Text: ""})
	if err == nil {
		t.Error("expected error sending empty message")
	}
}

func TestDaemonDeleteNonexistentSession(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	err := client.DeleteSession(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error deleting non-existent session")
	}
}

func TestDaemonMultipleEventSubscribers(t *testing.T) {
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Two subscribers.
	events1, err := client.SubscribeEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents 1: %v", err)
	}
	events2, err := client.SubscribeEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents 2: %v", err)
	}

	// Create a session.
	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Both should receive events.
	waitForEvent := func(ch <-chan agent.Event, name string) {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Errorf("%s: channel closed", name)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: timed out waiting for event", name)
		}
	}

	waitForEvent(events1, "subscriber1")
	waitForEvent(events2, "subscriber2")
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
	created, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test-project",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Newly created session should be unread (LastReadAt is zero).
	info, err := client.GetSession(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !info.Unread() {
		t.Error("expected new session to be unread")
	}

	// Mark the session as read.
	if err := client.MarkSessionRead(ctx, created.ID); err != nil {
		t.Fatalf("MarkSessionRead: %v", err)
	}

	// Session should now be read.
	info, err = client.GetSession(ctx, created.ID)
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

	err := client.MarkSessionRead(context.Background(), "nonexistent-id")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestDaemonMarkSessionReadThenUpdate(t *testing.T) {
	t.Parallel()
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()

	created, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test-project",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Mark as read.
	if err := client.MarkSessionRead(ctx, created.ID); err != nil {
		t.Fatalf("MarkSessionRead: %v", err)
	}

	// Verify it's read.
	info, err := client.GetSession(ctx, created.ID)
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
	info, err = client.GetSession(ctx, created.ID)
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
func testDaemonWithBackendAccess(t *testing.T) (*daemon.Daemon, *daemon.Client, func() *mockBackend, func()) {
	t.Helper()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)

	mgr := newMockBackendManager()
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)

	getBackend := func() *mockBackend {
		return mgr.getLatest()
	}

	cleanup := func() {
		d.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
	}

	return d, client, getBackend, cleanup
}

// receiveEvents collects up to n events from the channel, timing out after d.
func receiveEvents(ch <-chan agent.Event, n int, timeout time.Duration) []agent.Event {
	var result []agent.Event
	timer := time.After(timeout)
	for len(result) < n {
		select {
		case evt, ok := <-ch:
			if !ok {
				return result
			}
			result = append(result, evt)
		case <-timer:
			return result
		}
	}
	return result
}

// TestEventRoundTrip_StatusChange verifies that StatusChangeData survives
// the backend -> daemon SSE -> client JSON round-trip as a concrete type.
// This test is separate because the event originates from Start(), not injection.
func TestEventRoundTrip_StatusChange(t *testing.T) {
	_, client, _, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.SubscribeEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	received := receiveEvents(events, 3, 2*time.Second)

	var statusEvt *agent.Event
	for i := range received {
		if received[i].Type == agent.EventStatusChange {
			statusEvt = &received[i]
			break
		}
	}
	if statusEvt == nil {
		t.Fatalf("no status change event received; got %d events: %v", len(received), eventTypes(received))
	}

	data, ok := statusEvt.Data.(agent.StatusChangeData)
	if !ok {
		t.Fatalf("Event.Data type = %T, want agent.StatusChangeData", statusEvt.Data)
	}
	if data.OldStatus != agent.StatusStarting {
		t.Errorf("OldStatus = %q, want %q", data.OldStatus, agent.StatusStarting)
	}
	if data.NewStatus != agent.StatusBusy {
		t.Errorf("NewStatus = %q, want %q", data.NewStatus, agent.StatusBusy)
	}
}

// TestEventRoundTrip_InjectedEvents verifies that various event types survive
// the backend -> daemon SSE -> client JSON round-trip with correct concrete types.
func TestEventRoundTrip_InjectedEvents(t *testing.T) {
	tests := []struct {
		name  string
		event agent.Event
		check func(t *testing.T, evt agent.Event)
	}{
		{
			name: "PartUpdate",
			event: agent.Event{
				Type:      agent.EventPartUpdate,
				Timestamp: time.Now(),
				Data: agent.PartUpdateData{
					MessageID: "msg-123",
					Part: agent.Part{
						ID:   "part-456",
						Type: agent.PartText,
						Text: "Hello world",
					},
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.PartUpdateData)
				if !ok {
					t.Fatalf("Data type = %T, want PartUpdateData", evt.Data)
				}
				if data.MessageID != "msg-123" {
					t.Errorf("MessageID = %q, want %q", data.MessageID, "msg-123")
				}
				if data.Part.ID != "part-456" || data.Part.Type != agent.PartText || data.Part.Text != "Hello world" {
					t.Errorf("Part = %+v, unexpected", data.Part)
				}
			},
		},
		{
			name: "Message",
			event: agent.Event{
				Type:      agent.EventMessage,
				Timestamp: time.Now(),
				Data: agent.MessageData{
					Role:    "assistant",
					Content: "Here is my response",
					Parts: []agent.Part{
						{ID: "p1", Type: agent.PartText, Text: "Here is my response"},
					},
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.MessageData)
				if !ok {
					t.Fatalf("Data type = %T, want MessageData", evt.Data)
				}
				if data.Role != "assistant" || data.Content != "Here is my response" {
					t.Errorf("Role=%q Content=%q, unexpected", data.Role, data.Content)
				}
				if len(data.Parts) != 1 || data.Parts[0].ID != "p1" {
					t.Errorf("Parts = %+v, unexpected", data.Parts)
				}
			},
		},
		{
			name: "Permission",
			event: agent.Event{
				Type:      agent.EventPermission,
				Timestamp: time.Now(),
				Data: agent.PermissionData{
					RequestID:   "perm-789",
					Tool:        "bash",
					Description: "Run: rm -rf /",
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.PermissionData)
				if !ok {
					t.Fatalf("Data type = %T, want PermissionData", evt.Data)
				}
				if data.RequestID != "perm-789" || data.Tool != "bash" || data.Description != "Run: rm -rf /" {
					t.Errorf("Data = %+v, unexpected", data)
				}
			},
		},
		{
			name: "Error",
			event: agent.Event{
				Type:      agent.EventError,
				Timestamp: time.Now(),
				Data: agent.ErrorData{
					Message: "something went wrong",
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.ErrorData)
				if !ok {
					t.Fatalf("Data type = %T, want ErrorData", evt.Data)
				}
				if data.Message != "something went wrong" {
					t.Errorf("Message = %q, want %q", data.Message, "something went wrong")
				}
			},
		},
		{
			name: "ToolPartWithStatus",
			event: agent.Event{
				Type:      agent.EventPartUpdate,
				Timestamp: time.Now(),
				Data: agent.PartUpdateData{
					Part: agent.Part{
						ID:     "tool-1",
						Type:   agent.PartToolCall,
						Tool:   "bash",
						Text:   "ls -la",
						Status: agent.PartRunning,
					},
				},
			},
			check: func(t *testing.T, evt agent.Event) {
				data, ok := evt.Data.(agent.PartUpdateData)
				if !ok {
					t.Fatalf("Data type = %T, want PartUpdateData", evt.Data)
				}
				if data.Part.Tool != "bash" || data.Part.Status != agent.PartRunning || data.Part.Text != "ls -la" {
					t.Errorf("Part = %+v, unexpected", data.Part)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
			defer cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			events, err := client.SubscribeEvents(ctx)
			if err != nil {
				t.Fatalf("SubscribeEvents: %v", err)
			}

			_, err = client.CreateSession(ctx, agent.StartRequest{
				Backend:    agent.BackendOpenCode,
				ProjectDir: "/tmp/test",
				Prompt:     "hello",
			})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			time.Sleep(100 * time.Millisecond)
			receiveEvents(events, 3, 500*time.Millisecond) // drain initial events

			b := getBackend()
			if b == nil {
				t.Fatal("no backend created")
			}
			b.events <- tt.event

			received := receiveEvents(events, 1, 2*time.Second)
			if len(received) == 0 {
				t.Fatal("no event received")
			}
			if received[0].Type != tt.event.Type {
				t.Fatalf("event type = %q, want %q", received[0].Type, tt.event.Type)
			}
			tt.check(t, received[0])
		})
	}
}

// TestEventRoundTrip_StreamingTextDeltas verifies that multiple PartUpdate
// events with text deltas all arrive on the client with correct Part.ID and
// Part.Text, simulating streaming output from an agent.
func TestEventRoundTrip_StreamingTextDeltas(t *testing.T) {
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.SubscribeEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	receiveEvents(events, 3, 500*time.Millisecond)

	b := getBackend()
	if b == nil {
		t.Fatal("no backend created")
	}

	deltas := []string{"Hello", " world", "!"}
	for _, delta := range deltas {
		b.events <- agent.Event{
			Type:      agent.EventPartUpdate,
			Timestamp: time.Now(),
			Data: agent.PartUpdateData{
				Part: agent.Part{
					ID:   "text-part-1",
					Type: agent.PartText,
					Text: delta,
				},
			},
		}
	}

	received := receiveEvents(events, 3, 2*time.Second)
	if len(received) != 3 {
		t.Fatalf("expected 3 events, got %d", len(received))
	}

	for i, evt := range received {
		data, ok := evt.Data.(agent.PartUpdateData)
		if !ok {
			t.Errorf("event[%d].Data type = %T, want agent.PartUpdateData", i, evt.Data)
			continue
		}
		if data.Part.ID != "text-part-1" {
			t.Errorf("event[%d].Part.ID = %q, want %q", i, data.Part.ID, "text-part-1")
		}
		if data.Part.Text != deltas[i] {
			t.Errorf("event[%d].Part.Text = %q, want %q", i, data.Part.Text, deltas[i])
		}
	}
}

// TestEventRoundTrip_SessionID verifies that the daemon stamps SessionID
// on events before broadcasting, and that it survives the round-trip.
func TestEventRoundTrip_SessionID(t *testing.T) {
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.SubscribeEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	receiveEvents(events, 3, 500*time.Millisecond)

	b := getBackend()
	if b == nil {
		t.Fatal("no backend created")
	}

	b.events <- agent.Event{
		Type:      agent.EventPartUpdate,
		Timestamp: time.Now(),
		Data: agent.PartUpdateData{
			Part: agent.Part{ID: "x", Type: agent.PartText, Text: "hi"},
		},
	}

	received := receiveEvents(events, 1, 2*time.Second)
	if len(received) == 0 {
		t.Fatal("no event received")
	}
	if received[0].SessionID != info.ID {
		t.Errorf("SessionID = %q, want %q (daemon should stamp it)", received[0].SessionID, info.ID)
	}
}

// TestEventRoundTrip_TitleChange verifies that TitleChangeData survives the
// backend -> daemon SSE -> client JSON round-trip as a concrete type.
func TestEventRoundTrip_TitleChange(t *testing.T) {
	t.Parallel()
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := client.SubscribeEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	_, err = client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	receiveEvents(events, 3, 500*time.Millisecond) // drain initial events

	b := getBackend()
	if b == nil {
		t.Fatal("no backend created")
	}
	b.events <- agent.Event{
		Type:      agent.EventTitleChange,
		Timestamp: time.Now(),
		Data: agent.TitleChangeData{
			Title: "Fix authentication bug in login flow",
		},
	}

	received := receiveEvents(events, 1, 2*time.Second)
	if len(received) == 0 {
		t.Fatal("no event received")
	}
	if received[0].Type != agent.EventTitleChange {
		t.Fatalf("event type = %q, want %q", received[0].Type, agent.EventTitleChange)
	}
	data, ok := received[0].Data.(agent.TitleChangeData)
	if !ok {
		t.Fatalf("Data type = %T, want agent.TitleChangeData", received[0].Data)
	}
	if data.Title != "Fix authentication bug in login flow" {
		t.Errorf("Title = %q, want %q", data.Title, "Fix authentication bug in login flow")
	}
}

// TestDaemonTitleUpdateOnSession verifies that when a backend emits a title
// change event, the daemon updates the session info's Title field.
func TestDaemonTitleUpdateOnSession(t *testing.T) {
	t.Parallel()
	_, client, getBackend, cleanup := testDaemonWithBackendAccess(t)
	defer cleanup()

	ctx := context.Background()

	created, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test-project",
		Prompt:     "Fix the login bug",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify title is initially empty.
	info, err := client.GetSession(ctx, created.ID)
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
	info, err = client.GetSession(ctx, created.ID)
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

	_, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test-project",
		Prompt:     "Fix the login bug",
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
	sessions, err := client.ListSessions(ctx)
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

func eventTypes(events []agent.Event) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = string(e.Type)
	}
	return types
}

func TestDaemonListAgents(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)

	// OpenCode manager with agent listing support.
	ocMgr := &mockAgentListerManager{
		agents: func(ctx context.Context, projectDir string) ([]agent.AgentInfo, error) {
			return []agent.AgentInfo{
				{Name: "build", Description: "Build agent", Mode: "primary"},
				{Name: "plan", Description: "Plan agent", Mode: "primary"},
			}, nil
		},
	}
	d.BackendManagers[agent.BackendOpenCode] = ocMgr

	// Claude manager — no agent lister support.
	d.BackendManagers[agent.BackendClaudeCode] = newMockBackendManager()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer d.Stop()

	ctx := context.Background()

	// List agents for OpenCode backend.
	agents, err := client.ListAgents(ctx, agent.BackendOpenCode, "/tmp/test")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
	if agents[0].Name != "build" || agents[1].Name != "plan" {
		t.Errorf("unexpected agents: %+v", agents)
	}

	// List agents for Claude Code (no agent lister support).
	agents, err = client.ListAgents(ctx, agent.BackendClaudeCode, "/tmp/test")
	if err != nil {
		t.Fatalf("ListAgents for Claude Code: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents for Claude Code, got %d", len(agents))
	}
}

func TestDaemonListAgentsMissingParams(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	// Missing backend param — should return an error.
	_, err := client.ListAgents(ctx, "", "/tmp/test")
	if err == nil {
		t.Error("expected error for missing backend param")
	}
}

func TestDaemonListAgentsReturnsCachedFromStore(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")
	dbPath := filepath.Join(dir, "test.db")

	// Pre-seed the store with cached primary agents.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cachedAgents := []agent.AgentInfo{
		{Name: "build", Description: "Cached build", Mode: "primary"},
		{Name: "plan", Description: "Cached plan", Mode: "primary"},
	}
	if err := st.UpsertPrimaryAgents(agent.BackendOpenCode, "/tmp/test-proj", cachedAgents); err != nil {
		t.Fatalf("UpsertPrimaryAgents: %v", err)
	}

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.Store = st

	// Use an agent lister that tracks whether it was called synchronously.
	// The lister blocks until explicitly unblocked, so if the handler
	// returns before unblocking, we know it served from cache.
	listerCalled := make(chan struct{}, 1)
	ocMgr := &mockAgentListerManager{
		agents: func(ctx context.Context, projectDir string) ([]agent.AgentInfo, error) {
			listerCalled <- struct{}{}
			return []agent.AgentInfo{
				{Name: "build", Description: "Fresh build", Mode: "primary"},
				{Name: "plan", Description: "Fresh plan", Mode: "primary"},
				{Name: "debug", Description: "Fresh debug", Mode: "primary"},
			}, nil
		},
	}
	d.BackendManagers[agent.BackendOpenCode] = ocMgr
	d.BackendManagers[agent.BackendClaudeCode] = newMockBackendManager()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer func() {
		d.Stop()
		<-errCh
	}()

	ctx := context.Background()

	// Drain any background refresh triggered by warmAgentCaches (from
	// KnownProjectDirs finding sessions in the store — but we didn't
	// create any sessions, so this shouldn't fire).

	// Request agents — should return cached data immediately.
	agents, err := client.ListAgents(ctx, agent.BackendOpenCode, "/tmp/test-proj")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	// Should get the CACHED agents (2 agents, not 3).
	if len(agents) != 2 {
		t.Fatalf("expected 2 cached agents, got %d: %+v", len(agents), agents)
	}
	if agents[0].Description != "Cached build" {
		t.Errorf("expected cached description, got %q", agents[0].Description)
	}

	// The background refresh should have been triggered.
	select {
	case <-listerCalled:
		// Good — background refresh happened.
	case <-time.After(5 * time.Second):
		t.Error("expected background refresh to be triggered")
	}

	// After the refresh completes, subsequent requests should get the fresh data.
	time.Sleep(200 * time.Millisecond)

	agents, err = client.ListAgents(ctx, agent.BackendOpenCode, "/tmp/test-proj")
	if err != nil {
		t.Fatalf("ListAgents (2nd call): %v", err)
	}
	if len(agents) != 3 {
		t.Fatalf("expected 3 fresh agents after refresh, got %d: %+v", len(agents), agents)
	}
	if agents[0].Description != "Fresh build" {
		t.Errorf("expected fresh description, got %q", agents[0].Description)
	}
}

func TestDaemonListAgentsFallsBackToListerOnCacheMiss(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")
	dbPath := filepath.Join(dir, "test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// No pre-seeded agents — cache miss.

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.Store = st

	ocMgr := &mockAgentListerManager{
		agents: func(ctx context.Context, projectDir string) ([]agent.AgentInfo, error) {
			return []agent.AgentInfo{
				{Name: "build", Description: "Build agent", Mode: "primary"},
			}, nil
		},
	}
	d.BackendManagers[agent.BackendOpenCode] = ocMgr
	d.BackendManagers[agent.BackendClaudeCode] = newMockBackendManager()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer func() {
		d.Stop()
		<-errCh
	}()

	ctx := context.Background()

	// No cache — should fall back to synchronous lister call.
	agents, err := client.ListAgents(ctx, agent.BackendOpenCode, "/tmp/uncached-proj")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "build" {
		t.Errorf("unexpected agents: %+v", agents)
	}

	// After the synchronous call, the result should be persisted.
	cached, err := st.LoadPrimaryAgents(agent.BackendOpenCode, "/tmp/uncached-proj")
	if err != nil {
		t.Fatalf("LoadPrimaryAgents: %v", err)
	}
	if len(cached) != 1 || cached[0].Name != "build" {
		t.Errorf("expected persisted agents, got %+v", cached)
	}
}

func TestDaemonAgentStoredOnSession(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()

	// Create session with agent specified.
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test with agent",
		Agent:      "plan",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify agent is stored on session info.
	got, err := client.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Agent != "plan" {
		t.Errorf("session agent = %q, want %q", got.Agent, "plan")
	}

	// Send a message with a different agent — should update the session's agent.
	err = client.SendMessage(ctx, info.ID, agent.SendMessageOpts{Text: "follow up", Agent: "build"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	got, err = client.GetSession(ctx, info.ID)
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
func testDaemonWithDiscover(t *testing.T, snapshots []agent.SessionSnapshot) (*daemon.Daemon, *daemon.Client, func() *mockBackend, func()) {
	t.Helper()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)

	discMgr := &mockDiscovererManager{
		snapshots: snapshots,
	}
	// Custom create func to propagate sessionID from the request.
	discMgr.create = func(req agent.StartRequest) *mockBackend {
		b := newMockBackend()
		b.sessionID = req.SessionID
		return b
	}
	d.BackendManagers[agent.BackendOpenCode] = discMgr
	d.BackendManagers[agent.BackendClaudeCode] = &discMgr.mockBackendManager

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)

	getBackend := func() *mockBackend {
		return discMgr.getLatest()
	}

	cleanup := func() {
		d.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
	}

	return d, client, getBackend, cleanup
}

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

	_, client, _, cleanup := testDaemonWithDiscover(t, snapshots)
	defer cleanup()

	ctx := context.Background()

	// Trigger discovery.
	if err := client.DiscoverSessions(ctx, "/tmp/project-alpha"); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	// Both sessions should now appear in the list.
	sessions, err := client.ListSessions(ctx)
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

	_, client, _, cleanup := testDaemonWithDiscover(t, snapshots)
	defer cleanup()

	ctx := context.Background()

	// Discover twice.
	if err := client.DiscoverSessions(ctx, "/tmp/project-x"); err != nil {
		t.Fatalf("DiscoverSessions (1st): %v", err)
	}
	if err := client.DiscoverSessions(ctx, "/tmp/project-x"); err != nil {
		t.Fatalf("DiscoverSessions (2nd): %v", err)
	}

	// Should still only have 1 session (not duplicated).
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("expected 1 session after double-discover, got %d", len(sessions))
	}
}

func TestDiscoverSessionsSkipsManagedSessions(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)

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
	discMgr.create = func(req agent.StartRequest) *mockBackend {
		b := newMockBackend()
		b.sessionID = "oc-real-session"
		return b
	}
	d.BackendManagers[agent.BackendOpenCode] = discMgr
	d.BackendManagers[agent.BackendClaudeCode] = &discMgr.mockBackendManager

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer func() {
		d.Stop()
		<-errCh
	}()

	ctx := context.Background()

	// Create a real session first. runBackend will set ExternalID to "oc-real-session".
	_, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/project-z",
		Prompt:     "do stuff",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Wait for runBackend to capture the ExternalID.
	time.Sleep(200 * time.Millisecond)

	// Now discover — should NOT create a duplicate.
	if err := client.DiscoverSessions(ctx, "/tmp/project-z"); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	sessions, err := client.ListSessions(ctx)
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
			Directory: "/tmp/project-msg",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			UpdatedAt: time.Now().Add(-30 * time.Minute),
		},
	}

	_, client, getBackend, cleanup := testDaemonWithDiscover(t, snapshots)
	defer cleanup()

	ctx := context.Background()

	// Discover the historical session.
	if err := client.DiscoverSessions(ctx, "/tmp/project-msg"); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	// Find the discovered session's daemon ID.
	sessions, err := client.ListSessions(ctx)
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
	messages, err := client.GetSessionMessages(ctx, sessionID)
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
			Directory: "/tmp/project-resume",
			CreatedAt: time.Now().Add(-1 * time.Hour),
			UpdatedAt: time.Now().Add(-30 * time.Minute),
		},
	}

	_, client, getBackend, cleanup := testDaemonWithDiscover(t, snapshots)
	defer cleanup()

	ctx := context.Background()

	// Discover the historical session.
	if err := client.DiscoverSessions(ctx, "/tmp/project-resume"); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	sessions, err := client.ListSessions(ctx)
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
	err = client.SendMessage(ctx, sessionID, agent.SendMessageOpts{Text: "continue from here"})
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark as done.
	if err := client.SetVisibility(ctx, info.ID, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility(done): %v", err)
	}

	got, err := client.GetSession(ctx, info.ID)
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark as archived.
	if err := client.SetVisibility(ctx, info.ID, agent.VisibilityArchived); err != nil {
		t.Fatalf("SetVisibility(archived): %v", err)
	}

	got, err := client.GetSession(ctx, info.ID)
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark done, then revert to visible.
	if err := client.SetVisibility(ctx, info.ID, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility(done): %v", err)
	}
	if err := client.SetVisibility(ctx, info.ID, agent.VisibilityVisible); err != nil {
		t.Fatalf("SetVisibility(visible): %v", err)
	}

	got, err := client.GetSession(ctx, info.ID)
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

func TestDaemonSetVisibilityInvalid(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Invalid visibility value should fail.
	err = client.SetVisibility(ctx, info.ID, agent.SessionVisibility("invalid"))
	if err == nil {
		t.Error("expected error for invalid visibility value")
	}
}

func TestDaemonSetVisibilityNotFound(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	err := client.SetVisibility(context.Background(), "nonexistent-id", agent.VisibilityDone)
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestDaemonSendMessageClearsDoneVisibility(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark session as done.
	if err := client.SetVisibility(ctx, info.ID, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility(done): %v", err)
	}
	got, err := client.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Visibility != agent.VisibilityDone {
		t.Fatalf("visibility = %q, want %q", got.Visibility, agent.VisibilityDone)
	}

	// Send a follow-up message to the done session.
	if err := client.SendMessage(ctx, info.ID, agent.SendMessageOpts{Text: "follow up"}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Visibility should be reset to visible.
	got, err = client.GetSession(ctx, info.ID)
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Mark session as archived.
	if err := client.SetVisibility(ctx, info.ID, agent.VisibilityArchived); err != nil {
		t.Fatalf("SetVisibility(archived): %v", err)
	}
	got, err := client.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Visibility != agent.VisibilityArchived {
		t.Fatalf("visibility = %q, want %q", got.Visibility, agent.VisibilityArchived)
	}

	// Send a follow-up message to the archived session.
	if err := client.SendMessage(ctx, info.ID, agent.SendMessageOpts{Text: "follow up"}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	// Visibility should be reset to visible.
	got, err = client.GetSession(ctx, info.ID)
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Set a draft.
	if err := client.SetDraft(ctx, info.ID, "work in progress"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}

	got, err := client.GetSession(ctx, info.ID)
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Set then clear.
	if err := client.SetDraft(ctx, info.ID, "draft text"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}
	if err := client.SetDraft(ctx, info.ID, ""); err != nil {
		t.Fatalf("SetDraft(clear): %v", err)
	}

	got, err := client.GetSession(ctx, info.ID)
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

	err := client.SetDraft(context.Background(), "nonexistent-id", "draft")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestDaemonDraftVisibleInList(t *testing.T) {
	t.Parallel()
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := client.SetDraft(ctx, info.ID, "my draft"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}

	sessions, err := client.ListSessions(ctx)
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
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/test",
		Prompt:     "test prompt",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Set a draft.
	if err := client.SetDraft(ctx, info.ID, "my draft"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}

	// Send a message — should clear the draft.
	if err := client.SendMessage(ctx, info.ID, agent.SendMessageOpts{Text: "real message"}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	got, err := client.GetSession(ctx, info.ID)
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
// stop the daemon.
func testDaemonWithStore(t *testing.T, dir string) (d *daemon.Daemon, client *daemon.Client, sockPath, pidPath, dbPath string, cleanup func()) {
	t.Helper()

	if dir == "" {
		dir = shortTempDir(t)
	}
	sockPath = filepath.Join(dir, "test.sock")
	pidPath = filepath.Join(dir, "test.pid")
	dbPath = filepath.Join(dir, "test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	d = daemon.NewWithPaths(sockPath, pidPath)
	d.Store = st

	// Wire up mock backend manager.
	mgr := newMockBackendManager()
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client = daemon.NewClient(sockPath)
	waitForDaemon(t, client)

	cleanup = func() {
		d.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
	}
	return
}

func TestPersistence_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)

	// --- Phase 1: create sessions and mutate user-owned fields ---

	d1, client1, sockPath, pidPath, dbPath, cleanup1 := testDaemonWithStore(t, dir)
	_ = d1
	ctx := context.Background()

	// Create a session.
	info, err := client1.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/myproject",
		Prompt:     "fix the bug",
		TicketID:   "TICKET-42",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Wait for backend to start.
	time.Sleep(150 * time.Millisecond)

	// Mutate user-owned fields.
	if _, err := client1.ToggleFollowUp(ctx, info.ID); err != nil {
		t.Fatalf("ToggleFollowUp: %v", err)
	}
	if err := client1.SetVisibility(ctx, info.ID, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	if err := client1.SetDraft(ctx, info.ID, "my draft text"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}
	if err := client1.MarkSessionRead(ctx, info.ID); err != nil {
		t.Fatalf("MarkSessionRead: %v", err)
	}

	// Snapshot the session before stopping.
	before, err := client1.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	// Stop daemon 1.
	cleanup1()

	// --- Phase 2: restart daemon with same DB, verify persistence ---

	// Need to remove stale socket before new daemon can listen.
	os.Remove(sockPath)
	os.Remove(pidPath)

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open (phase 2): %v", err)
	}
	d2 := daemon.NewWithPaths(sockPath, pidPath)
	d2.Store = st2
	mgr2 := newMockBackendManager()
	d2.BackendManagers[agent.BackendOpenCode] = mgr2
	d2.BackendManagers[agent.BackendClaudeCode] = mgr2

	errCh2 := make(chan error, 1)
	go func() { errCh2 <- d2.Run() }()

	client2 := daemon.NewClient(sockPath)
	waitForDaemon(t, client2)

	defer func() {
		d2.Stop()
		select {
		case <-errCh2:
		case <-time.After(5 * time.Second):
			t.Error("daemon 2 did not stop in time")
		}
	}()

	// The session should survive the restart.
	sessions, err := client2.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session after restart, got %d", len(sessions))
	}

	after := sessions[0]

	// Verify identity.
	if after.ID != before.ID {
		t.Errorf("ID mismatch: %s vs %s", after.ID, before.ID)
	}

	// Verify user-owned fields survived.
	if !after.FollowUp {
		t.Error("FollowUp should be true after restart")
	}
	if after.Visibility != agent.VisibilityDone {
		t.Errorf("Visibility = %q, want %q", after.Visibility, agent.VisibilityDone)
	}
	if after.Draft != "my draft text" {
		t.Errorf("Draft = %q, want %q", after.Draft, "my draft text")
	}
	if after.LastReadAt.IsZero() {
		t.Error("LastReadAt should not be zero")
	}

	// Verify backend-owned fields survived.
	if after.ProjectDir != "/tmp/myproject" {
		t.Errorf("ProjectDir = %q, want %q", after.ProjectDir, "/tmp/myproject")
	}
	if after.TicketID != "TICKET-42" {
		t.Errorf("TicketID = %q, want %q", after.TicketID, "TICKET-42")
	}
}

func TestPersistence_DeleteSurvivesRestart(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)

	// Phase 1: create and delete a session.
	_, client1, sockPath, pidPath, dbPath, cleanup1 := testDaemonWithStore(t, dir)
	ctx := context.Background()

	info, err := client1.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/proj",
		Prompt:     "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if err := client1.DeleteSession(ctx, info.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	cleanup1()

	// Phase 2: restart, verify session is gone.
	os.Remove(sockPath)
	os.Remove(pidPath)

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	d2 := daemon.NewWithPaths(sockPath, pidPath)
	d2.Store = st2

	errCh := make(chan error, 1)
	go func() { errCh <- d2.Run() }()

	client2 := daemon.NewClient(sockPath)
	waitForDaemon(t, client2)

	defer func() {
		d2.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
		st2.Close()
	}()

	sessions, err := client2.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions after delete+restart, got %d", len(sessions))
	}
}

// TestPersistence_StaleBusyStatusNormalizedOnRestart verifies that sessions
// persisted with a busy or starting status are normalized to idle when the
// daemon restarts. Without this fix, the inbox shows an infinite spinner for
// sessions that were interrupted by a daemon restart.
func TestPersistence_StaleBusyStatusNormalizedOnRestart(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)

	// Phase 1: create a session and leave it in a busy state.
	d1, client1, sockPath, pidPath, dbPath, cleanup1 := testDaemonWithStore(t, dir)
	_ = d1
	ctx := context.Background()

	info, err := client1.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/stale-proj",
		Prompt:     "do something",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	// Verify the session is busy (mockBackend transitions to busy on Start).
	session, err := client1.GetSession(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session.Status != agent.StatusBusy {
		t.Fatalf("expected status=busy before shutdown, got %s", session.Status)
	}

	// Kill daemon without letting the backend transition to idle.
	cleanup1()

	// Phase 2: restart daemon — the session should be normalized to idle.
	os.Remove(sockPath)
	os.Remove(pidPath)

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	d2 := daemon.NewWithPaths(sockPath, pidPath)
	d2.Store = st2
	mgr2 := newMockBackendManager()
	d2.BackendManagers[agent.BackendOpenCode] = mgr2
	d2.BackendManagers[agent.BackendClaudeCode] = mgr2

	errCh := make(chan error, 1)
	go func() { errCh <- d2.Run() }()

	client2 := daemon.NewClient(sockPath)
	waitForDaemon(t, client2)

	defer func() {
		d2.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
		st2.Close()
	}()

	sessions, err := client2.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session after restart, got %d", len(sessions))
	}
	if sessions[0].Status != agent.StatusIdle {
		t.Errorf("expected status=idle after restart, got %s (stale busy status was not normalized)", sessions[0].Status)
	}
}

// TestDiscoverSessions_NormalizesStaleStatusOnRediscover verifies that
// rediscovery normalizes stale busy/starting statuses for backend-less
// sessions.
func TestDiscoverSessions_NormalizesStaleStatusOnRediscover(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	now := time.Now()

	snapshots := []agent.SessionSnapshot{
		{
			ID:        "ext-stale-1",
			Title:     "Stale session",
			Directory: "/tmp/stale-project",
			CreatedAt: now.Add(-1 * time.Hour),
			UpdatedAt: now,
		},
	}

	// Phase 1: create daemon with store, discover session, then
	// manually corrupt the status by writing busy to the DB.
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")
	dbPath := filepath.Join(dir, "test.db")

	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	d1 := daemon.NewWithPaths(sockPath, pidPath)
	d1.Store = st1
	discMgr1 := &mockDiscovererManager{snapshots: snapshots}
	d1.BackendManagers[agent.BackendOpenCode] = discMgr1
	d1.BackendManagers[agent.BackendClaudeCode] = &discMgr1.mockBackendManager

	errCh1 := make(chan error, 1)
	go func() { errCh1 <- d1.Run() }()
	client1 := daemon.NewClient(sockPath)
	waitForDaemon(t, client1)

	ctx := context.Background()
	if err := client1.DiscoverSessions(ctx, "/tmp/stale-project"); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	sessions1, err := client1.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions1) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions1))
	}
	if sessions1[0].Status != agent.StatusIdle {
		t.Fatalf("expected idle after discover, got %s", sessions1[0].Status)
	}

	// Corrupt the persisted status to simulate a stale busy state
	// (as if the daemon had been killed while the session was active).
	corruptedInfo := sessions1[0]
	corruptedInfo.Status = agent.StatusBusy
	if err := st1.UpsertSession(corruptedInfo); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	d1.Stop()
	<-errCh1
	st1.Close()

	// Phase 2: restart and re-discover. The stale busy status
	// should be normalized to idle.
	os.Remove(sockPath)
	os.Remove(pidPath)

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	d2 := daemon.NewWithPaths(sockPath, pidPath)
	d2.Store = st2
	discMgr2 := &mockDiscovererManager{snapshots: snapshots}
	d2.BackendManagers[agent.BackendOpenCode] = discMgr2
	d2.BackendManagers[agent.BackendClaudeCode] = &discMgr2.mockBackendManager

	errCh2 := make(chan error, 1)
	go func() { errCh2 <- d2.Run() }()
	client2 := daemon.NewClient(sockPath)
	waitForDaemon(t, client2)

	defer func() {
		d2.Stop()
		select {
		case <-errCh2:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
		st2.Close()
	}()

	// After restart, the session loaded from DB should already be idle
	// (normalized on load).
	sessions2, err := client2.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions (after restart): %v", err)
	}
	if len(sessions2) != 1 {
		t.Fatalf("expected 1 session after restart, got %d", len(sessions2))
	}
	if sessions2[0].Status != agent.StatusIdle {
		t.Errorf("expected status=idle after restart, got %s", sessions2[0].Status)
	}

	// Re-discover — should also not revert to stale status.
	if err := client2.DiscoverSessions(ctx, "/tmp/stale-project"); err != nil {
		t.Fatalf("DiscoverSessions (phase 2): %v", err)
	}

	sessions3, err := client2.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions (after re-discover): %v", err)
	}
	if len(sessions3) != 1 {
		t.Fatalf("expected 1 session after re-discover, got %d", len(sessions3))
	}
	if sessions3[0].Status != agent.StatusIdle {
		t.Errorf("expected status=idle after re-discover, got %s (rediscovery did not normalize stale status)", sessions3[0].Status)
	}
}

func TestPersistence_NilStoreDoesNotPanic(t *testing.T) {
	t.Parallel()

	// The standard testDaemon helper does NOT set a store.
	// This test verifies the nil-safe path doesn't panic.
	_, client, cleanup := testDaemon(t)
	defer cleanup()

	ctx := context.Background()
	info, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/proj",
		Prompt:     "hello",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Mutate through all persist paths — none should panic.
	_, _ = client.ToggleFollowUp(ctx, info.ID)
	_ = client.SetVisibility(ctx, info.ID, agent.VisibilityArchived)
	_ = client.SetDraft(ctx, info.ID, "draft")
	_ = client.MarkSessionRead(ctx, info.ID)
	_ = client.SendMessage(ctx, info.ID, agent.SendMessageOpts{Text: "msg"})
	time.Sleep(100 * time.Millisecond)
	_ = client.DeleteSession(ctx, info.ID)
}

func TestPersistence_DiscoverMergePreservesUserFields(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	now := time.Now()

	// Phase 1: discover a session and set user-owned fields.
	snapshots := []agent.SessionSnapshot{
		{
			ID:        "ext-merge-1",
			Title:     "Original title",
			Directory: "/tmp/merge-project",
			CreatedAt: now.Add(-2 * time.Hour),
			UpdatedAt: now.Add(-1 * time.Hour),
		},
	}

	d1, client1, sockPath, pidPath, dbPath, cleanup1 := testDaemonWithStore(t, dir)
	discMgr1 := &mockDiscovererManager{snapshots: snapshots}
	d1.BackendManagers[agent.BackendOpenCode] = discMgr1
	ctx := context.Background()

	if err := client1.DiscoverSessions(ctx, "/tmp/merge-project"); err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}

	sessions, err := client1.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	sessionID := sessions[0].ID

	// Set user-owned fields on the discovered session.
	if _, err := client1.ToggleFollowUp(ctx, sessionID); err != nil {
		t.Fatalf("ToggleFollowUp: %v", err)
	}
	if err := client1.SetVisibility(ctx, sessionID, agent.VisibilityDone); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	if err := client1.SetDraft(ctx, sessionID, "my followup draft"); err != nil {
		t.Fatalf("SetDraft: %v", err)
	}

	cleanup1()

	// Phase 2: restart daemon, re-discover with updated backend fields.
	os.Remove(sockPath)
	os.Remove(pidPath)

	updatedSnapshots := []agent.SessionSnapshot{
		{
			ID:        "ext-merge-1",                // same external ID
			Title:     "Updated title from backend", // backend changed the title
			Directory: "/tmp/merge-project",
			CreatedAt: now.Add(-2 * time.Hour),
			UpdatedAt: now, // backend has a newer UpdatedAt
		},
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	d2 := daemon.NewWithPaths(sockPath, pidPath)
	d2.Store = st2
	discMgr2 := &mockDiscovererManager{snapshots: updatedSnapshots}
	d2.BackendManagers[agent.BackendOpenCode] = discMgr2
	d2.BackendManagers[agent.BackendClaudeCode] = &discMgr2.mockBackendManager

	errCh2 := make(chan error, 1)
	go func() { errCh2 <- d2.Run() }()

	client2 := daemon.NewClient(sockPath)
	waitForDaemon(t, client2)
	defer func() {
		d2.Stop()
		select {
		case <-errCh2:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
	}()

	// Re-discover — the session should be a duplicate (already loaded from DB).
	if err := client2.DiscoverSessions(ctx, "/tmp/merge-project"); err != nil {
		t.Fatalf("DiscoverSessions (phase 2): %v", err)
	}

	sessions2, err := client2.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions2) != 1 {
		t.Fatalf("expected 1 session after re-discover, got %d", len(sessions2))
	}

	merged := sessions2[0]

	// Backend-owned fields should be updated from the new snapshot.
	if merged.Title != "Updated title from backend" {
		t.Errorf("Title = %q, want %q", merged.Title, "Updated title from backend")
	}

	// User-owned fields should be preserved from the DB.
	if !merged.FollowUp {
		t.Error("FollowUp should be true (preserved from DB)")
	}
	if merged.Visibility != agent.VisibilityDone {
		t.Errorf("Visibility = %q, want %q", merged.Visibility, agent.VisibilityDone)
	}
	if merged.Draft != "my followup draft" {
		t.Errorf("Draft = %q, want %q", merged.Draft, "my followup draft")
	}
}

func TestDaemonDebugOpenCodeServers(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)

	// Use a real OpenCodeBackendManager with a fake startServerFn so the
	// reconciler populates the servers map without spawning real processes.
	ocMgr := daemon.NewOpenCodeBackendManager()
	ocMgr.ServerManager().SetStartServerFn(func(ctx context.Context, projectDir string) (*agent.OpenCodeServer, error) {
		return &agent.OpenCodeServer{
			URL:        "http://127.0.0.1:54321",
			ProjectDir: projectDir,
			StartedAt:  time.Now(),
		}, nil
	})

	d.BackendManagers[agent.BackendOpenCode] = ocMgr
	d.BackendManagers[agent.BackendClaudeCode] = newMockBackendManager()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer d.Stop()

	ctx := context.Background()

	// Create a session so the server gets started via GetOrStartServer.
	_, err := client.CreateSession(ctx, agent.StartRequest{
		Backend:    agent.BackendOpenCode,
		ProjectDir: "/tmp/project-a",
		Prompt:     "test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Wait for the server to appear (reconciler may need a tick).
	var servers []daemon.ServerStatus
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		servers, err = client.ListOpenCodeServers(ctx)
		if err != nil {
			t.Fatalf("ListOpenCodeServers: %v", err)
		}
		if len(servers) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	srv := servers[0]
	if srv.ProjectDir != "/tmp/project-a" {
		t.Errorf("project dir = %q, want /tmp/project-a", srv.ProjectDir)
	}
	if srv.URL != "http://127.0.0.1:54321" {
		t.Errorf("URL = %q, want http://127.0.0.1:54321", srv.URL)
	}
	if srv.SessionCount != 1 {
		t.Errorf("session count = %d, want 1", srv.SessionCount)
	}
}

func TestDaemonDebugOpenCodeServersEmpty(t *testing.T) {
	t.Parallel()
	// When no OpenCode backend manager is registered, the endpoint
	// should return an empty list, not an error.
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.BackendManagers[agent.BackendOpenCode] = newMockBackendManager()

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer d.Stop()

	servers, err := client.ListOpenCodeServers(context.Background())
	if err != nil {
		t.Fatalf("ListOpenCodeServers: %v", err)
	}
	if len(servers) != 0 {
		t.Errorf("expected 0 servers, got %d", len(servers))
	}
}

func TestDaemonSearchSessions(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")
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
			ProjectDir: "/tmp/proj", ProjectName: "myproject",
			Title: "Fix authentication bug", Prompt: "fix login",
			CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour),
		},
		{
			ID: "ses-s2", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			ProjectDir: "/tmp/proj", ProjectName: "myproject",
			Title: "Add dark mode", Prompt: "implement dark mode toggle",
			CreatedAt: now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour),
		},
		{
			ID: "ses-s3", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			ProjectDir: "/tmp/other", ProjectName: "otherproject",
			Title: "Refactor database layer", Prompt: "clean up db queries",
			CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := st.UpsertSession(info); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	}

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.Store = st
	mgr := newMockBackendManager()
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer func() {
		d.Stop()
		<-errCh
	}()

	ctx := context.Background()
	search := func(q string) agent.SearchParams { return agent.SearchParams{Query: q} }

	// --- Substring AND matching ---

	// Substring match: "authentication" appears in ses-s1 title.
	results, err := client.SearchSessions(ctx, search("authentication"))
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
	results, err = client.SearchSessions(ctx, search("dark toggle"))
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
	results, err = client.SearchSessions(ctx, search("dark queries"))
	if err != nil {
		t.Fatalf("SearchSessions(dark queries): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for 'dark queries', got %d", len(results))
	}

	// Case insensitive: "DATABASE" matches "database" in ses-s3.
	results, err = client.SearchSessions(ctx, search("DATABASE"))
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
	results, err = client.SearchSessions(ctx, search("auth|dark"))
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
	results, err = client.SearchSessions(ctx, search("auth bug|database layer"))
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
	results, err = client.SearchSessions(ctx, search("xyznotfound|dark"))
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
	results, err = client.SearchSessions(ctx, search("myproject"))
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
	results, err = client.SearchSessions(ctx, agent.SearchParams{
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
	results, err = client.SearchSessions(ctx, agent.SearchParams{
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
	results, err = client.SearchSessions(ctx, agent.SearchParams{
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
	results, err = client.SearchSessions(ctx, agent.SearchParams{
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

	results, err = client.SearchSessions(ctx, search("xyznotfound"))
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
	_, err := client.SearchSessions(ctx, agent.SearchParams{})
	if err == nil {
		t.Fatal("expected error when all search params are empty")
	}
}

func TestDaemonSearchSessionsVisibility(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")
	dbPath := filepath.Join(dir, "test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	for _, info := range []agent.SessionInfo{
		{
			ID: "ses-active", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			ProjectDir: "/tmp/proj", ProjectName: "proj",
			Title: "active session", Prompt: "do stuff",
			Visibility: agent.VisibilityVisible,
			CreatedAt:  now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour),
		},
		{
			ID: "ses-done", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			ProjectDir: "/tmp/proj", ProjectName: "proj",
			Title: "done session", Prompt: "finished task",
			Visibility: agent.VisibilityDone,
			CreatedAt:  now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
		},
		{
			ID: "ses-archived", Backend: agent.BackendOpenCode, Status: agent.StatusIdle,
			ProjectDir: "/tmp/proj", ProjectName: "proj",
			Title: "archived session", Prompt: "old stuff",
			Visibility: agent.VisibilityArchived,
			CreatedAt:  now.Add(-1 * time.Hour), UpdatedAt: now.Add(-1 * time.Hour),
		},
	} {
		if err := st.UpsertSession(info); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	}

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.Store = st
	mgr := newMockBackendManager()
	d.BackendManagers[agent.BackendOpenCode] = mgr
	d.BackendManagers[agent.BackendClaudeCode] = mgr

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)
	defer func() {
		d.Stop()
		<-errCh
	}()

	ctx := context.Background()

	// Default visibility (empty): only active sessions. Must provide at
	// least one search param for the HTTP handler, so use a broad query.
	results, err := client.SearchSessions(ctx, agent.SearchParams{Query: "session"})
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
	results, err = client.SearchSessions(ctx, agent.SearchParams{
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
	results, err = client.SearchSessions(ctx, agent.SearchParams{
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
	results, err = client.SearchSessions(ctx, agent.SearchParams{
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

func waitForDaemon(t *testing.T, client *daemon.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if err := client.Ping(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("daemon did not start in time")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
