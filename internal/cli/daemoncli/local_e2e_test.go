package daemoncli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host"
	hostmux "github.com/acksell/clank/internal/host/mux"
	hoststore "github.com/acksell/clank/internal/host/store"
	"github.com/acksell/clank/pkg/auth"
	"github.com/acksell/clank/pkg/gateway"
	"github.com/acksell/clank/pkg/provisioner"
)

// TestLocalE2E_TUICreatesSession_AndFetches drives the full local-mode
// wire end to end: a daemonclient (TUI's HTTP client) talks to a gateway,
// which talks to an in-process host service backed by a stub backend
// manager. POST /sessions must return a SessionInfo with a non-empty ID
// (the host generates the ID; pre-PR-3 the hub did) and GET /sessions/{id}
// must return that same SessionInfo augmented with the live backend's
// runtime status.
//
// This test exists because the PR 3 hub deletion silently broke the
// "session_id is required" contract on POST /sessions — the host's
// create handler still expected a hub-assigned ID after the hub was
// removed. The fix moves ID generation onto the host. This regression
// test pins that contract.
func TestLocalE2E_TUICreatesSession_AndFetches(t *testing.T) {
	t.Parallel()

	// Stub backend so the test never touches opencode/claude.
	stub := &stubBackendManager{}

	repo := initTestGitRepo(t)

	// Real host store at a temp DB so handleGetSession's
	// GetSessionMetadata path is exercised.
	dbPath := filepath.Join(t.TempDir(), "host.db")
	hs, err := hoststore.Open(dbPath)
	if err != nil {
		t.Fatalf("hoststore.Open: %v", err)
	}
	t.Cleanup(func() { hs.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: stub,
		},
		SessionsStore: hs,
	})
	t.Cleanup(svc.Shutdown)

	hostHTTP := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(hostHTTP.Close)

	// Gateway in front of the host. AllowAll resolves every request to
	// the "local" laptop user — file-perms gate the unix socket in
	// production; tests just inject the principal directly.
	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: &fixedHostProvisioner{
			url:       hostHTTP.URL,
			transport: http.DefaultTransport,
		},
	}, nil)
	if err != nil {
		t.Fatalf("gateway.NewGateway: %v", err)
	}
	gwSrv := httptest.NewServer(auth.Middleware(gw.Handler(), &auth.AllowAll{UserID: "local"}))
	t.Cleanup(gwSrv.Close)

	// daemonclient is the TUI's HTTP client; same one production wires.
	cli := daemonclient.NewTCPClient(gwSrv.URL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	created, err := cli.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: repo, WorktreeID: "01JV7T7F9Y6XQ1R6M8R2W4K3NZ"},
		Prompt:  "hello",
	})
	if err != nil {
		t.Fatalf("Sessions().Create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("Sessions().Create returned SessionInfo with empty ID")
	}
	if created.Backend != agent.BackendOpenCode {
		t.Errorf("Backend: got %q, want %q", created.Backend, agent.BackendOpenCode)
	}
	if created.Prompt != "hello" {
		t.Errorf("Prompt: got %q, want %q", created.Prompt, "hello")
	}

	// The host must dispatch the initial prompt via OpenAndSend during
	// the create handler. This is the contract the hub used to own
	// (and that PR 3 silently broke until phase 6 — the symptom was
	// "session not started" / spinning busy with no agent reply). We
	// pin it here so the regression cannot return.
	stub.last.mu.Lock()
	openAndSend := stub.last.openAndSend
	gotText := stub.last.sendOpts.Text
	stub.last.mu.Unlock()
	if !openAndSend {
		t.Error("handleCreateSession did not call OpenAndSend on the backend")
	}
	if gotText != "hello" {
		t.Errorf("OpenAndSend received text %q, want %q", gotText, "hello")
	}

	// GET /sessions/{id} must return SessionInfo (not the lightweight
	// SessionSnapshot) so the TUI's Session(id).Get() works.
	got, err := cli.Session(created.ID).Get(ctx)
	if err != nil {
		t.Fatalf("Session(%s).Get: %v", created.ID, err)
	}
	if got.ID != created.ID {
		t.Errorf("ID round-trip: got %q, want %q", got.ID, created.ID)
	}
	if got.Backend != agent.BackendOpenCode {
		t.Errorf("Backend round-trip: got %q, want %q", got.Backend, agent.BackendOpenCode)
	}

	// Host-scoped routes go through /hosts/{hostname}/... in the TUI's
	// HostClient. The gateway must strip that prefix before forwarding,
	// otherwise the host returns 404 "page not found" — that was the
	// symptom for the "connect provider" modal failing in the wild.
	// The stub backend doesn't wire up an opencode auth manager so the
	// handler itself errors with "auth manager is not configured" — but
	// reaching that error proves routing through /hosts/{name}/auth
	// succeeded (the alternative is a generic 404 from the mux).
	_, authErr := cli.Host("local").ListAuthProviders(ctx)
	if authErr == nil {
		t.Error("expected an error from stub host (no auth manager wired)")
	} else if strings.Contains(authErr.Error(), "404 page not found") {
		t.Errorf("gateway did not strip /hosts/{name} prefix; got 404: %v", authErr)
	}
}

// fixedHostProvisioner returns the same HostRef on every EnsureHost. The
// test wires it to the in-process host's HTTP test server.
type fixedHostProvisioner struct {
	url       string
	transport http.RoundTripper
}

func (f *fixedHostProvisioner) EnsureHost(context.Context, string) (provisioner.HostRef, error) {
	return provisioner.HostRef{URL: f.url, Transport: f.transport, Hostname: "local"}, nil
}
func (*fixedHostProvisioner) SuspendHost(context.Context, string) error { return nil }
func (*fixedHostProvisioner) DestroyHost(context.Context, string) error { return nil }

// stubBackendManager spawns a stubBackend on every CreateBackend. The
// shared `last` field lets tests inspect what the most recently created
// backend received (e.g. did handleCreateSession actually dispatch the
// initial prompt via OpenAndSend?).
type stubBackendManager struct {
	mu   sync.Mutex
	last *stubBackend
}

func (m *stubBackendManager) Init(_ context.Context, _ func() ([]string, error)) error { return nil }
func (m *stubBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	b := &stubBackend{
		events: make(chan agent.Event, 16),
		done:   make(chan struct{}),
	}
	m.mu.Lock()
	m.last = b
	m.mu.Unlock()
	return b, nil
}
func (m *stubBackendManager) Shutdown() {}

type stubBackend struct {
	events chan agent.Event
	// done closes when Stop runs. PushEvent guards on it so a test
	// goroutine racing service-Cleanup never panics on send-to-
	// closed-channel and the race detector stays quiet.
	done chan struct{}
	// pendingPushes counts in-flight PushEvent calls so Stop can wait
	// for them to finish before closing the events channel — without
	// it, a PushEvent that has passed its done-check but hasn't
	// reached the send still races with close(events).
	pendingPushes sync.WaitGroup
	stopOnce      sync.Once

	mu          sync.Mutex
	openCalled  bool
	sendOpts    agent.SendMessageOpts
	openAndSend bool

	// Tests override the runtime fields a real backend usually
	// updates from inside Open/Start. Both fields are protected by
	// b.mu and read by Status()/SessionID() on the host's relay
	// goroutine, so concurrent test mutation is safe.
	statusOverride agent.SessionStatus
	statusSet      bool
	idOverride     string
	idSet          bool

	// Records mutating operations so wire-level tests can assert the
	// host translated the request correctly without mocking yet
	// another struct.
	aborted          bool
	stopped          bool
	revertID         string
	forkID           string
	permissionID     string
	permissionAllow  bool
	permissionCalled bool
}

// PushEvent injects an event into the backend's events channel as if
// the agent emitted it. Drops the event if Stop has been called so
// test goroutines that race the service's Cleanup don't panic on
// send-to-closed-channel (and don't trip the race detector).
func (b *stubBackend) PushEvent(evt agent.Event) {
	select {
	case <-b.done:
		return
	default:
	}
	b.pendingPushes.Add(1)
	defer b.pendingPushes.Done()
	select {
	case b.events <- evt:
	case <-b.done:
	}
}

// SetExternalID overrides what SessionID() returns going forward.
// Tests use this to mimic opencode's late-binding behavior — the real
// session ID isn't known until Open completes.
func (b *stubBackend) SetExternalID(id string) {
	b.mu.Lock()
	b.idOverride = id
	b.idSet = true
	b.mu.Unlock()
}

// SetStatus overrides what Status() returns going forward.
func (b *stubBackend) SetStatus(s agent.SessionStatus) {
	b.mu.Lock()
	b.statusOverride = s
	b.statusSet = true
	b.mu.Unlock()
}

func (b *stubBackend) Open(context.Context) error {
	b.mu.Lock()
	b.openCalled = true
	b.mu.Unlock()
	return nil
}
func (b *stubBackend) OpenAndSend(_ context.Context, opts agent.SendMessageOpts) error {
	b.mu.Lock()
	b.openCalled = true
	b.openAndSend = true
	b.sendOpts = opts
	b.mu.Unlock()
	return nil
}
func (b *stubBackend) Send(_ context.Context, opts agent.SendMessageOpts) error {
	b.mu.Lock()
	b.sendOpts = opts
	b.mu.Unlock()
	return nil
}
func (b *stubBackend) Abort(context.Context) error {
	b.mu.Lock()
	b.aborted = true
	b.mu.Unlock()
	return nil
}
func (b *stubBackend) Stop() error {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.stopped = true
		b.mu.Unlock()
		// Close done first so any PushEvent that hasn't entered the
		// send select bails out via its preflight check. Wait for
		// any push that's already past the preflight to finish
		// before closing events — that's the only safe way to
		// guarantee no concurrent send.
		close(b.done)
		b.pendingPushes.Wait()
		close(b.events)
	})
	return nil
}
func (b *stubBackend) Status() agent.SessionStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.statusSet {
		return b.statusOverride
	}
	return agent.StatusIdle
}
func (b *stubBackend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.idSet {
		return b.idOverride
	}
	return "stub-ext-id"
}
func (*stubBackend) Messages(context.Context) ([]agent.MessageData, error) { return nil, nil }
func (b *stubBackend) Revert(_ context.Context, msgID string) error {
	b.mu.Lock()
	b.revertID = msgID
	b.mu.Unlock()
	return nil
}
func (b *stubBackend) Fork(_ context.Context, msgID string) (agent.ForkResult, error) {
	b.mu.Lock()
	b.forkID = msgID
	b.mu.Unlock()
	return agent.ForkResult{ID: "ext-forked-" + msgID}, nil
}
func (b *stubBackend) RespondPermission(_ context.Context, permissionID string, allow bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.permissionID = permissionID
	b.permissionAllow = allow
	b.permissionCalled = true
	return nil
}
func (b *stubBackend) Events() <-chan agent.Event                          { return b.events }

// initTestGitRepo creates a git repo with an "origin" remote so
// host.workDirFor accepts the LocalPath as a usable repo root.
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	run("git", "remote", "add", "origin", "git@example.com:acme/repo.git")
	run("git", "commit", "--allow-empty", "-m", "initial")
	return dir
}
