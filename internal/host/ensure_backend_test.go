package host_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hoststore "github.com/acksell/clank/internal/host/store"
)

// failingOpenBackend mirrors noopBackend except Open returns the
// provided error every time. CreateBackend on its manager returns a
// new instance per call so the test can observe teardown.
type failingOpenBackend struct {
	openErr error
	stopped bool
}

func (b *failingOpenBackend) Open(_ context.Context) error { return b.openErr }
func (b *failingOpenBackend) OpenAndSend(_ context.Context, _ agent.SendMessageOpts) error {
	return b.openErr
}
func (b *failingOpenBackend) Send(_ context.Context, _ agent.SendMessageOpts) error { return nil }
func (b *failingOpenBackend) Abort(_ context.Context) error                         { return nil }
func (b *failingOpenBackend) Stop() error {
	b.stopped = true
	return nil
}
func (b *failingOpenBackend) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (b *failingOpenBackend) Status() agent.SessionStatus { return agent.StatusError }
func (b *failingOpenBackend) SessionID() string           { return "" }
func (b *failingOpenBackend) Messages(_ context.Context) ([]agent.MessageData, error) {
	return nil, nil
}
func (b *failingOpenBackend) Revert(_ context.Context, _ string) error { return nil }
func (b *failingOpenBackend) Fork(_ context.Context, _ string) (agent.ForkResult, error) {
	return agent.ForkResult{}, nil
}
func (b *failingOpenBackend) RespondPermission(_ context.Context, _ string, _ bool, _ string) error {
	return nil
}

func (b *failingOpenBackend) RespondQuestion(_ context.Context, _ string, _ []agent.QuestionAnswer, _ bool) error {
	return nil
}

type failingOpenBackendManager struct {
	openErr     error
	created     []*failingOpenBackend
	createCalls int
}

func (m *failingOpenBackendManager) Init(_ context.Context, _ func() ([]string, error)) error {
	return nil
}
func (m *failingOpenBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	m.createCalls++
	b := &failingOpenBackend{openErr: m.openErr}
	m.created = append(m.created, b)
	return b, nil
}
func (m *failingOpenBackendManager) Shutdown() {}

// TestEnsureBackend_OpenFailureTearsDownRegistration pins the contract
// that a failing Open() does NOT leave a registered-but-broken backend
// in the live registry. CR found the prior comment claimed Send still
// works after Open failure — it doesn't (SessionBackend contract
// requires Open first), so leaving the wrapper around forces a daemon
// restart for the user to recover.
func TestEnsureBackend_OpenFailureTearsDownRegistration(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := hoststore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	repo := initGitRepo(t, "git@example.com:acme/repo.git")
	bogusOpen := errors.New("simulated open failure")
	mgr := &failingOpenBackendManager{openErr: bogusOpen}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: mgr,
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 5*1_000_000_000) // 5s
	defer cancel()

	const id = "01OPENFAILURE0000000001"
	if err := st.UpsertSession(ctx, agent.SessionInfo{
		ID:      id,
		Backend: agent.BackendOpenCode,
		Status:  agent.StatusIdle,
		GitRef:  agent.GitRef{LocalPath: repo},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// First call: ensureBackend → Open fails → teardown.
	_, _, err = svc.OpenSession(ctx, id)
	if err == nil {
		t.Fatal("expected Open failure to surface, got nil")
	}
	if !errors.Is(err, bogusOpen) {
		t.Errorf("expected %v in error chain, got %v", bogusOpen, err)
	}

	// The torn-down backend must NOT linger in the registry; otherwise
	// the next call would find the broken wrapper and Send would fast-
	// fail with "session not open" forever.
	if _, ok := svc.Session(id); ok {
		t.Fatal("backend remained in live registry after Open failure; teardown did not run")
	}
	if len(mgr.created) != 1 || !mgr.created[0].stopped {
		t.Errorf("expected the spawned backend to be Stopped; created=%d, stopped[0]=%v", len(mgr.created), mgr.created[0].stopped)
	}

	// Second call: ensureBackend re-runs CreateBackend instead of
	// returning the lingering broken wrapper. The user's retry path
	// works.
	_, _, err = svc.OpenSession(ctx, id)
	if err == nil {
		t.Fatal("expected second Open to fail too (manager still set to fail)")
	}
	if mgr.createCalls != 2 {
		t.Errorf("CreateBackend should run on every retry after teardown; got %d calls, want 2", mgr.createCalls)
	}
}

// TestEnsureBackend_NotFoundIsErrNotFound pins the success criterion
// for "session is in neither the live registry nor the store": the
// caller gets ErrNotFound (mapped to 404 by the mux), not a wrapped
// store error.
func TestEnsureBackend_NotFoundIsErrNotFound(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := hoststore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx := context.Background()
	err = svc.SendMessage(ctx, "01DOESNOTEXIST00000000", agent.SendMessageOpts{Text: "x"})
	if !errors.Is(err, host.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown session, got %v", err)
	}
}

// wedgeableBackend opens cleanly, then can be flipped "dead" to model a CLI
// subprocess/transport that dropped after a successful open (e.g. fallout from an
// instant interrupt). Once dead, Status() reports StatusDead and Send() fails the
// way the real backend does ("client not connected").
type wedgeableBackend struct {
	mu      sync.Mutex
	dead    bool
	stopped bool
}

func (b *wedgeableBackend) setDead() {
	b.mu.Lock()
	b.dead = true
	b.mu.Unlock()
}
func (b *wedgeableBackend) isDead() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dead
}
func (b *wedgeableBackend) wasStopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped
}

func (b *wedgeableBackend) Open(_ context.Context) error { return nil }
func (b *wedgeableBackend) OpenAndSend(_ context.Context, _ agent.SendMessageOpts) error {
	return nil
}
func (b *wedgeableBackend) Send(_ context.Context, _ agent.SendMessageOpts) error {
	if b.isDead() {
		return errors.New("client not connected")
	}
	return nil
}
func (b *wedgeableBackend) Abort(_ context.Context) error { return nil }
func (b *wedgeableBackend) Stop() error {
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()
	return nil
}
func (b *wedgeableBackend) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (b *wedgeableBackend) Status() agent.SessionStatus {
	if b.isDead() {
		return agent.StatusDead
	}
	return agent.StatusIdle
}
func (b *wedgeableBackend) SessionID() string { return "" }
func (b *wedgeableBackend) Messages(_ context.Context) ([]agent.MessageData, error) {
	return nil, nil
}
func (b *wedgeableBackend) Revert(_ context.Context, _ string) error { return nil }
func (b *wedgeableBackend) Fork(_ context.Context, _ string) (agent.ForkResult, error) {
	return agent.ForkResult{}, nil
}
func (b *wedgeableBackend) RespondPermission(_ context.Context, _ string, _ bool, _ string) error {
	return nil
}

func (b *wedgeableBackend) RespondQuestion(_ context.Context, _ string, _ []agent.QuestionAnswer, _ bool) error {
	return nil
}

type rehydrateBackendManager struct {
	mu          sync.Mutex
	created     []*wedgeableBackend
	createCalls int
}

func (m *rehydrateBackendManager) Init(_ context.Context, _ func() ([]string, error)) error {
	return nil
}
func (m *rehydrateBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	b := &wedgeableBackend{}
	m.created = append(m.created, b)
	return b, nil
}
func (m *rehydrateBackendManager) Shutdown() {}

// TestEnsureBackend_DeadBackendIsRehydrated reproduces the "needs attention"
// wedge from the host side: after a session's backend connection dies, the
// cached wrapper lingers in the live registry, so every follow-up /message is
// served by ensureBackend returning that dead backend — the user's send bounces
// forever with no recovery path. The chat-client spec deliberately omits /stop
// and /open and relies on the SSE/messages paths to "lazily rehydrate"
// (05-operations.md); honoring that contract means ensureBackend MUST drop a
// dead cached backend and recreate it, exactly as it already does after an
// Open() failure (see TestEnsureBackend_OpenFailureTearsDownRegistration).
func TestEnsureBackend_DeadBackendIsRehydrated(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "host.db")
	st, err := hoststore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	repo := initGitRepo(t, "git@example.com:acme/repo.git")
	mgr := &rehydrateBackendManager{}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: mgr,
		},
		SessionsStore: st,
	})
	t.Cleanup(svc.Shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 5*1_000_000_000) // 5s
	defer cancel()

	const id = "01DEADBACKEND00000000001"
	if err := st.UpsertSession(ctx, agent.SessionInfo{
		ID:      id,
		Backend: agent.BackendOpenCode,
		Status:  agent.StatusIdle,
		GitRef:  agent.GitRef{LocalPath: repo},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// First send: ensureBackend creates + opens backend #1, which serves it fine.
	if err := svc.SendMessage(ctx, id, agent.SendMessageOpts{Text: "hello"}); err != nil {
		t.Fatalf("first SendMessage: %v", err)
	}
	mgr.mu.Lock()
	first := mgr.created[0]
	mgr.mu.Unlock()

	// The backend's connection dies (CLI subprocess exited after an interrupt).
	first.setDead()

	// Second send: ensureBackend must NOT hand back the dead backend. It must
	// drop it and rehydrate a fresh one so the user's retry works.
	if err := svc.SendMessage(ctx, id, agent.SendMessageOpts{Text: "are you there?"}); err != nil {
		t.Fatalf("second SendMessage after backend died: got %v, want nil (session must rehydrate)", err)
	}

	mgr.mu.Lock()
	calls := mgr.createCalls
	mgr.mu.Unlock()
	if calls != 2 {
		t.Errorf("CreateBackend calls = %d, want 2 (dead backend must be rehydrated, not reused)", calls)
	}
	if !first.wasStopped() {
		t.Error("the dead backend must be Stopped when dropped from the registry")
	}
}
