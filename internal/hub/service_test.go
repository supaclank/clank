package hub_test

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	forked            bool
	forkedMessageID   string
	permissionReplied bool
	permissionID      string
	permissionAllow   bool

	// history is returned by Messages(). Tests can set it to control
	// the message history returned by the backend.
	history []agent.MessageData

	// onOpenAndSend is called during OpenAndSend, allowing tests to
	// control behavior (e.g. simulate a long-running prompt).
	onOpenAndSend func(ctx context.Context, opts agent.SendMessageOpts) error
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		events:    make(chan agent.Event, 64),
		status:    agent.StatusStarting,
		sessionID: "mock-session-1",
	}
}

func (m *mockBackend) Open(ctx context.Context) error {
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
	return nil
}

func (m *mockBackend) OpenAndSend(ctx context.Context, opts agent.SendMessageOpts) error {
	if err := m.Open(ctx); err != nil {
		return err
	}
	if m.onOpenAndSend != nil {
		return m.onOpenAndSend(ctx, opts)
	}
	// NOTE: do not append to m.messages — that field tracks follow-up
	// Send() calls explicitly. Initial prompts go through OpenAndSend
	// and are tracked separately by tests that care.
	return nil
}

func (m *mockBackend) Watch(ctx context.Context) error {
	return nil
}

func (m *mockBackend) Send(ctx context.Context, opts agent.SendMessageOpts) error {
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
	already := m.stopped
	m.stopped = true
	m.mu.Unlock()
	if already {
		// idempotent: avoid panic from closing the channel twice
		return nil
	}
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

func (m *mockBackend) Fork(ctx context.Context, messageID string) (agent.ForkResult, error) {
	m.mu.Lock()
	m.forked = true
	m.forkedMessageID = messageID
	m.mu.Unlock()
	return agent.ForkResult{ID: "forked-external-id", Title: "Forked session"}, nil
}

func (m *mockBackend) RespondPermission(ctx context.Context, permissionID string, allow bool) error {
	m.mu.Lock()
	m.permissionReplied = true
	m.permissionID = permissionID
	m.permissionAllow = allow
	m.mu.Unlock()
	return nil
}

// mockBackendManager implements agent.BackendManager for testing. It wraps
// a creation function so tests can intercept and track created backends.
type mockBackendManager struct {
	mu      sync.Mutex
	create  func(inv agent.BackendInvocation) *mockBackend // custom creation logic; nil = newMockBackend()
	latest  *mockBackend                                   // last created backend
	all     []*mockBackend                                 // all created backends
	stopped bool
}

func newMockBackendManager() *mockBackendManager {
	return &mockBackendManager{}
}

func (m *mockBackendManager) CreateBackend(_ context.Context, inv agent.BackendInvocation) (agent.SessionBackend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b *mockBackend
	if m.create != nil {
		b = m.create(inv)
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

// startHubOnSocket runs s on a fresh Unix listener in a temp directory
// and blocks until the daemon is responsive. It mirrors what
// daemoncli.runHubServer does in production minus the PID file and
// signal handling — both of which are out of scope for hub_test.
//
// Returns a connected client, the socket path (so persistence tests
// can restart on the same path via startHubAtSocket), and a cleanup
// function that stops the daemon and waits for Run to return.
func startHubOnSocket(t *testing.T, s *hub.Service) (*hubclient.Client, string, func()) {
	t.Helper()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")
	client, cleanup := startHubAtSocket(t, s, sockPath)
	return client, sockPath, cleanup
}

// startHubAtSocket runs s on a Unix listener bound to the supplied
// sockPath. If s has not had a host client injected, this helper
// constructs an in-process host.Service from s.BackendManagers, wraps
// it in an httptest.Server through hostmux, and registers the resulting
// *hostclient.HTTP — preserving the production wire shape (Hub→HTTP→Host)
// without adding a fake or in-process shortcut on the Hub side.
func startHubAtSocket(t *testing.T, s *hub.Service, sockPath string) (*hubclient.Client, func()) {
	t.Helper()

	// Spin a real host.Service behind an httptest server when the
	// caller hasn't already wired one. The fixture's lifetime is
	// owned here (closed in cleanup) — Hub no longer owns any host
	// process or service.
	hostFixture := ensureHostFixture(t, s)

	// Defensive: clear any stale socket left by a previous instance
	// (closing a Unix listener does not unlink the on-disk file).
	os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(listener, hubmux.New(s, nil).Handler()) }()

	client := hubclient.NewClient(sockPath)
	waitForDaemon(t, client)

	cleanup := func() {
		s.Stop()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop in time")
		}
		if hostFixture != nil {
			hostFixture.close()
		}
		os.Remove(sockPath)
	}
	return client, cleanup
}

// hostTestFixture ties together the in-process host.Service and the
// httptest.Server fronting it, so the test cleanup path can tear both
// down in the right order.
type hostTestFixture struct {
	svc       *host.Service
	srv       *httptest.Server
	client    *hostclient.HTTP
	clonesDir string // tempdir backing host.Options.ClonesDir
}

func (f *hostTestFixture) close() {
	if f == nil {
		return
	}
	_ = f.client.Close()
	f.srv.Close()
	f.svc.Shutdown()
}

// hostFixturesByHub maps a *hub.Service to the host fixture seeded by
// ensureHostFixture so package-level helpers (registerTestRepoAt) can
// reach the underlying *host.Service to call AddRepo directly. The
// previous wire-level RegisterRepo path is gone (§7.5: the host adds
// implicitly during CreateSession), so test seeding now goes through
// the same in-process API tests would use.
var hostFixturesByHub sync.Map // map[*hub.Service]*hostTestFixture

// ensureHostFixture builds an HTTP-fronted host.Service from
// s.BackendManagers and registers it as the local host on s. If the
// caller already injected a host client (e.g. repos_test.go which
// supplies its own) the helper is a no-op and returns nil.
func ensureHostFixture(t *testing.T, s *hub.Service) *hostTestFixture {
	t.Helper()
	if _, ok := s.Host("local"); ok {
		return nil
	}
	clonesDir := t.TempDir()
	svc := host.New(host.Options{BackendManagers: s.BackendManagers, ClonesDir: clonesDir})
	// Run() boots the backend reconciler (warm-start servers, etc.).
	// Tests don't supply a knownDirs callback, so reconciler runs
	// against an empty set — same as the production path before any
	// project is opened.
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("host.Init: %v", err)
	}
	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	c := hostclient.NewHTTP(srv.URL, nil)
	s.SetHostClient(c)
	f := &hostTestFixture{svc: svc, srv: srv, client: c, clonesDir: clonesDir}
	hostFixturesByHub.Store(s, f)
	t.Cleanup(func() { hostFixturesByHub.Delete(s) })
	return f
}

// testDaemon creates a daemon with mock backends, starts it, registers
// a default test repo on the local host, and returns the daemon, a
// connected client, and a cleanup function. Tests that issue
// CreateSession can use the package-level testRemoteURL constant
// without having to set up the repo themselves.
func testDaemon(t *testing.T) (*hub.Service, *hubclient.Client, func()) {
	t.Helper()

	s := hub.New()
	mgr := newMockBackendManager()
	s.BackendManagers[agent.BackendOpenCode] = mgr
	s.BackendManagers[agent.BackendClaudeCode] = mgr

	client, _, cleanup := startHubOnSocket(t, s)
	registerTestRepo(t, s)
	return s, client, cleanup
}

// --- Tests ---

func testDaemonWithBackendAccess(t *testing.T) (*hub.Service, *hubclient.Client, func() *mockBackend, func()) {
	t.Helper()

	s := hub.New()
	mgr := newMockBackendManager()
	s.BackendManagers[agent.BackendOpenCode] = mgr
	s.BackendManagers[agent.BackendClaudeCode] = mgr

	client, _, cleanup := startHubOnSocket(t, s)
	registerTestRepo(t, s)
	getBackend := func() *mockBackend { return mgr.getLatest() }
	return s, client, getBackend, cleanup
}

// receiveEvents collects up to n events from the channel, timing out after s.
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
func eventTypes(events []agent.Event) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = string(e.Type)
	}
	return types
}

func testDaemonWithDiscover(t *testing.T, snapshots []agent.SessionSnapshot) (*hub.Service, *hubclient.Client, func() *mockBackend, string, func()) {
	t.Helper()

	s := hub.New()

	discMgr := &mockDiscovererManager{
		snapshots: snapshots,
	}
	// Custom create func to propagate sessionID from the request.
	discMgr.create = func(inv agent.BackendInvocation) *mockBackend {
		b := newMockBackend()
		b.sessionID = inv.ResumeExternalID
		return b
	}
	s.BackendManagers[agent.BackendOpenCode] = discMgr
	s.BackendManagers[agent.BackendClaudeCode] = &discMgr.mockBackendManager

	client, _, cleanup := startHubOnSocket(t, s)
	repoDir := registerTestRepo(t, s)
	getBackend := func() *mockBackend { return discMgr.getLatest() }
	return s, client, getBackend, repoDir, cleanup
}

// testDaemonWithStore is the persistence variant: it allocates a temp
// dir (or uses the supplied one), opens a SQLite store, and starts a
// hub bound to a socket inside that dir. Returns the socket path and DB
// path so two-phase persistence tests can shut the daemon down and
// restart a second instance on the same artifacts.
//
// PID file path is intentionally NOT returned: hub.Service no longer
// touches a PID file (Phase 2F lifted that into daemoncli), so tests
// have nothing to clean up there.
func testDaemonWithStore(t *testing.T, dir string) (s *hub.Service, client *hubclient.Client, sockPath, dbPath, repoDir string, cleanup func()) {
	t.Helper()

	if dir == "" {
		dir = shortTempDir(t)
	}
	sockPath = filepath.Join(dir, "test.sock")
	dbPath = filepath.Join(dir, "test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	s = hub.New()
	s.Store = st

	// Wire up mock backend manager.
	mgr := newMockBackendManager()
	s.BackendManagers[agent.BackendOpenCode] = mgr
	s.BackendManagers[agent.BackendClaudeCode] = mgr

	client, cleanup = startHubAtSocket(t, s, sockPath)
	repoDir = registerTestRepo(t, s)
	return
}

func waitForDaemon(t *testing.T, client *hubclient.Client) {
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

// --- Git helpers for merge tests ---

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func gitWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

// initGitRepo creates a fresh git repo with an initial commit.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "user.name", "Test")
	gitWriteFile(t, filepath.Join(dir, "README.md"), "# test\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "initial commit")
	return dir
}

// initGitRepoAt creates a fresh git repo at the given dir (creating
// parents as needed) with an `origin` remote pointing at remoteURL.
// Used by registerTestRepoAtWithRef to seed the host's deterministic
// clone path before tests that resolve Remote refs.
func initGitRepoAt(t *testing.T, dir, remoteURL string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "user.name", "Test")
	gitRun(t, dir, "remote", "add", "origin", remoteURL)
	gitWriteFile(t, filepath.Join(dir, "README.md"), "# test\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "initial commit")
}
