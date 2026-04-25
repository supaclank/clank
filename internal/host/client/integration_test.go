package hostclient_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostclient "github.com/acksell/clank/internal/host/client"
	hostmux "github.com/acksell/clank/internal/host/mux"
)

// stubBackend is a programmable SessionBackend used only as a test
// fixture for the wire-layer SUT (mux + http client + SSE bridge). It
// is not a mock of the system under test.
type stubBackend struct {
	events chan agent.Event
	// id is returned by SessionID(). For the real opencode backend this
	// is empty at construction and only populated after Start creates
	// the remote session; use idAfterStart to simulate that pattern.
	id           string
	idAfterStart string
	status       agent.SessionStatus
	startedReq   agent.StartRequest
	sentMsg      agent.SendMessageOpts
	aborted      bool
	stopped      bool
}

func newStubBackend(id string) *stubBackend {
	return &stubBackend{
		events: make(chan agent.Event, 16),
		id:     id,
		status: agent.StatusIdle,
	}
}

func (b *stubBackend) Open(_ context.Context) error {
	if b.idAfterStart != "" {
		b.id = b.idAfterStart
	}
	return nil
}
func (b *stubBackend) OpenAndSend(ctx context.Context, opts agent.SendMessageOpts) error {
	if err := b.Open(ctx); err != nil {
		return err
	}
	return b.Send(ctx, opts)
}
func (b *stubBackend) Send(_ context.Context, o agent.SendMessageOpts) error {
	b.sentMsg = o
	return nil
}
func (b *stubBackend) Abort(_ context.Context) error                           { b.aborted = true; return nil }
func (b *stubBackend) Stop() error                                             { b.stopped = true; close(b.events); return nil }
func (b *stubBackend) Events() <-chan agent.Event                              { return b.events }
func (b *stubBackend) Status() agent.SessionStatus                             { return b.status }
func (b *stubBackend) SessionID() string                                       { return b.id }
func (b *stubBackend) Messages(_ context.Context) ([]agent.MessageData, error) { return nil, nil }
func (b *stubBackend) Revert(_ context.Context, _ string) error                { return nil }
func (b *stubBackend) Fork(_ context.Context, _ string) (agent.ForkResult, error) {
	return agent.ForkResult{ID: "forked-" + b.id, Title: "Forked"}, nil
}
func (b *stubBackend) RespondPermission(_ context.Context, _ string, _ bool) error { return nil }

// stubManager is a BackendManager that returns the same stubBackend per
// CreateBackend call (held in `next`).
type stubManager struct {
	next *stubBackend
}

func (m *stubManager) Init(_ context.Context, _ func() ([]string, error)) error { return nil }
func (m *stubManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	return m.next, nil
}
func (m *stubManager) Shutdown() {}

const testRemoteURL = "git@github.com:acksell/clank.git"

// initGitRepo creates a real git repo with an "origin" remote so the host
// can resolve the RepoRef → RepoID and pass CreateSession's checks.
func initGitRepo(t *testing.T) string {
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
	run("git", "remote", "add", "origin", testRemoteURL)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "initial")
	return dir
}

// TestHTTPRoundTrip_CreateSessionAndEvents validates the full wire
// path: Service → hostmux → httptest server → HTTP hostclient →
// httpSessionBackend. It checks that:
//   - CreateSession returns a SessionBackend whose external ID matches
//     the underlying stub.
//   - Events emitted by the host-side stub are decoded and delivered
//     through SSE to the client-side channel.
//   - StatusChange events update the cached client-side status.
func TestHTTPRoundTrip_CreateSessionAndEvents(t *testing.T) {
	t.Parallel()

	stub := newStubBackend("ext-123")
	mgr := &stubManager{next: stub}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: mgr,
		},
	})
	t.Cleanup(svc.Shutdown)

	dir := initGitRepo(t)

	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)

	c := hostclient.NewHTTP(srv.URL, nil)
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	be, _, err := c.Sessions().Create(ctx, "sid-1", agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got := be.SessionID(); got != "ext-123" {
		t.Errorf("SessionID = %q, want ext-123", got)
	}

	// Subscribe to events; the SSE goroutine starts on first call.
	ch := be.Events()

	// Push an event from the host side; expect it to arrive on the client.
	stub.events <- agent.Event{
		Type:      agent.EventStatusChange,
		SessionID: "ext-123",
		Timestamp: time.Now().UTC(),
		Data:      agent.StatusChangeData{OldStatus: agent.StatusIdle, NewStatus: agent.StatusBusy},
	}

	select {
	case ev := <-ch:
		if ev.Type != agent.EventStatusChange {
			t.Errorf("event type = %q, want %q", ev.Type, agent.EventStatusChange)
		}
		if ev.SessionID != "ext-123" {
			t.Errorf("event session_id = %q, want ext-123", ev.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	// Status() should reflect the StatusChange we just propagated.
	// Allow a short window for the event handler to update the cache.
	deadline := time.Now().Add(500 * time.Millisecond)
	for be.Status() != agent.StatusBusy && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := be.Status(); got != agent.StatusBusy {
		t.Errorf("Status() = %q, want %q", got, agent.StatusBusy)
	}

	// Stop the host-side backend — the SSE stream should close and
	// the events channel should drain to closed.
	if err := svc.StopSession("sid-1"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case _, ok := <-ch:
			if !ok {
				return // success: channel closed
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("events channel did not close after StopSession")
}

// TestHTTPRoundTrip_SendMessageAndAbort exercises the simple POST
// endpoints for completeness.
func TestHTTPRoundTrip_SendMessageAndAbort(t *testing.T) {
	t.Parallel()

	stub := newStubBackend("ext-x")
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &stubManager{next: stub},
		},
	})
	t.Cleanup(svc.Shutdown)
	dir := initGitRepo(t)
	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)
	c := hostclient.NewHTTP(srv.URL, nil)
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	be, _, err := c.Sessions().Create(ctx, "sid-2", agent.StartRequest{
		Backend: agent.BackendOpenCode, GitRef: agent.GitRef{LocalPath: dir}, Prompt: "hi",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := be.Send(ctx, agent.SendMessageOpts{Text: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if stub.sentMsg.Text != "hello" {
		t.Errorf("stub.sentMsg.Text = %q, want %q", stub.sentMsg.Text, "hello")
	}
	if err := be.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !stub.aborted {
		t.Error("stub.aborted = false, want true")
	}
}

// TestHTTPRoundTrip_StartPopulatesExternalID is a regression test for a
// bug where the client-side httpSessionBackend returned a stale empty
// ExternalID forever because:
//
//  1. POST /sessions (CreateBackend) returns before the backend has
//     opened a real remote session, so the response body carries
//     ExternalID="".
//  2. POST /sessions/{id}/start then drives the backend to open its
//     remote session (which is where the real opencode sessionID
//     becomes known), but the host handler returned 204 No Content,
//     so the client never learned the new ExternalID.
//
// Result: SessionBackend.SessionID() kept returning "" on the Hub even
// after a successful Start, so the Hub persisted external_id="" on the
// session row — causing duplicate rows to be created from
// discoverSessions on next TUI open / daemon restart.
//
// The fix: Start returns a SessionSnapshot body; the client updates
// its cached externalID and status from the response.
func TestHTTPRoundTrip_StartPopulatesExternalID(t *testing.T) {
	t.Parallel()

	// Mirror how opencode's real backend only learns its sessionID
	// after the remote session is opened inside Start().
	stub := newStubBackend("")
	stub.idAfterStart = "ext-late"
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &stubManager{next: stub},
		},
	})
	t.Cleanup(svc.Shutdown)
	dir := initGitRepo(t)
	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)
	c := hostclient.NewHTTP(srv.URL, nil)
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	be, _, err := c.Sessions().Create(ctx, "sid-late", agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "hi",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Before Start the stub still returns "" for SessionID, so the
	// client's cached externalID is also "".
	if got := be.SessionID(); got != "" {
		t.Errorf("before Start: SessionID = %q, want empty", got)
	}

	if err := be.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// After Open the host backend knows its external ID; the client
	// must learn it from the Open response.
	if got := be.SessionID(); got != "ext-late" {
		t.Errorf("after Open: SessionID = %q, want ext-late", got)
	}
}

// TestHTTPRoundTrip_NotFound checks that the HTTP client maps a 404
// response back to host.ErrNotFound (errors.Is equality).
func TestHTTPRoundTrip_NotFound(t *testing.T) {
	t.Parallel()

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &stubManager{},
		},
	})
	t.Cleanup(svc.Shutdown)
	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)
	c := hostclient.NewHTTP(srv.URL, nil)
	t.Cleanup(func() { _ = c.Close() })

	err := c.Session("does-not-exist").Stop(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, host.ErrNotFound) {
		t.Errorf("err is not host.ErrNotFound: %v", err)
	}
}
