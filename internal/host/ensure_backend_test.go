package host_test

import (
	"context"
	"errors"
	"path/filepath"
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

func (b *failingOpenBackend) Open(_ context.Context) error                  { return b.openErr }
func (b *failingOpenBackend) OpenAndSend(_ context.Context, _ agent.SendMessageOpts) error {
	return b.openErr
}
func (b *failingOpenBackend) Send(_ context.Context, _ agent.SendMessageOpts) error { return nil }
func (b *failingOpenBackend) Abort(_ context.Context) error                          { return nil }
func (b *failingOpenBackend) Stop() error {
	b.stopped = true
	return nil
}
func (b *failingOpenBackend) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (b *failingOpenBackend) Status() agent.SessionStatus              { return agent.StatusError }
func (b *failingOpenBackend) SessionID() string                        { return "" }
func (b *failingOpenBackend) Messages(_ context.Context) ([]agent.MessageData, error) {
	return nil, nil
}
func (b *failingOpenBackend) Revert(_ context.Context, _ string) error { return nil }
func (b *failingOpenBackend) Fork(_ context.Context, _ string) (agent.ForkResult, error) {
	return agent.ForkResult{}, nil
}
func (b *failingOpenBackend) RespondPermission(_ context.Context, _ string, _ bool) error {
	return nil
}

type failingOpenBackendManager struct {
	openErr  error
	created  []*failingOpenBackend
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
