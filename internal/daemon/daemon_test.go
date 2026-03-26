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
)

// mockBackend implements agent.Backend for testing.
type mockBackend struct {
	mu        sync.Mutex
	events    chan agent.Event
	status    agent.SessionStatus
	sessionID string
	started   bool
	stopped   bool
	messages  []string
	aborted   bool

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

func (m *mockBackend) SendMessage(ctx context.Context, text string) error {
	m.mu.Lock()
	m.messages = append(m.messages, text)
	m.mu.Unlock()

	// Emit a message event.
	m.events <- agent.Event{
		Type:      agent.EventMessage,
		Timestamp: time.Now(),
		Data: agent.MessageData{
			Role:    "user",
			Content: text,
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

	// Wire up mock backend factory.
	var lastBackend *mockBackend
	var backendMu sync.Mutex
	d.BackendFactory = func(bt agent.BackendType, req agent.StartRequest) (agent.Backend, error) {
		b := newMockBackend()
		backendMu.Lock()
		lastBackend = b
		backendMu.Unlock()
		return b, nil
	}

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
	_ = lastBackend

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
	var latestBackend *mockBackend
	var backendMu sync.Mutex

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.BackendFactory = func(bt agent.BackendType, req agent.StartRequest) (agent.Backend, error) {
		b := newMockBackend()
		backendMu.Lock()
		latestBackend = b
		backendMu.Unlock()
		return b, nil
	}

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

	err = client.SendMessage(ctx, info.ID, "follow-up message")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Verify the backend received the message.
	backendMu.Lock()
	b := latestBackend
	backendMu.Unlock()

	time.Sleep(50 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.messages) != 1 || b.messages[0] != "follow-up message" {
		t.Errorf("expected messages=[follow-up message], got %v", b.messages)
	}
}

func TestDaemonAbortSession(t *testing.T) {
	var latestBackend *mockBackend
	var backendMu sync.Mutex

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.BackendFactory = func(bt agent.BackendType, req agent.StartRequest) (agent.Backend, error) {
		b := newMockBackend()
		backendMu.Lock()
		latestBackend = b
		backendMu.Unlock()
		return b, nil
	}

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

	backendMu.Lock()
	b := latestBackend
	backendMu.Unlock()

	time.Sleep(50 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.aborted {
		t.Error("expected backend to be aborted")
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
	var backends []*mockBackend
	var backendMu sync.Mutex

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	pidPath := filepath.Join(dir, "test.pid")

	d := daemon.NewWithPaths(sockPath, pidPath)
	d.BackendFactory = func(bt agent.BackendType, req agent.StartRequest) (agent.Backend, error) {
		b := newMockBackend()
		backendMu.Lock()
		backends = append(backends, b)
		backendMu.Unlock()
		return b, nil
	}

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
	backendMu.Lock()
	defer backendMu.Unlock()
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
	err := client.SendMessage(ctx, "nonexistent", "hello")
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

	err = client.SendMessage(ctx, info.ID, "")
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

	var latestBackend *mockBackend
	var backendMu sync.Mutex
	d.BackendFactory = func(bt agent.BackendType, req agent.StartRequest) (agent.Backend, error) {
		b := newMockBackend()
		backendMu.Lock()
		latestBackend = b
		backendMu.Unlock()
		return b, nil
	}

	errCh := make(chan error, 1)
	go func() { errCh <- d.Run() }()

	client := daemon.NewClient(sockPath)
	waitForDaemon(t, client)

	getBackend := func() *mockBackend {
		backendMu.Lock()
		defer backendMu.Unlock()
		return latestBackend
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

// --- Helpers ---

func eventTypes(events []agent.Event) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = string(e.Type)
	}
	return types
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
