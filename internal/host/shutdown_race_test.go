package host_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// slowBackendManager is a fixture (not a mock) whose CreateBackend blocks
// on a release channel so tests can drive a precise race between
// CreateSession and Shutdown. Init/Shutdown are atomic and observable so
// the test can assert ordering.
type slowBackendManager struct {
	mu          sync.Mutex
	shutdown    bool
	createEntry chan struct{} // closed once when CreateBackend is reached
	release     chan struct{} // CreateBackend returns once this is closed
	once        sync.Once
}

func newSlowBackendManager() *slowBackendManager {
	return &slowBackendManager{
		createEntry: make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (m *slowBackendManager) Init(_ context.Context, _ func() ([]string, error)) error {
	return nil
}

func (m *slowBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	m.once.Do(func() { close(m.createEntry) })
	<-m.release
	return &noopBackend{}, nil
}

func (m *slowBackendManager) Shutdown() {
	m.mu.Lock()
	m.shutdown = true
	m.mu.Unlock()
}

func (m *slowBackendManager) wasShutdown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shutdown
}

// TestService_ShutdownDuringCreateSession_DoesNotLeakBackend pins down a
// race: if Shutdown runs while CreateSession is blocked inside
// mgr.CreateBackend (after the live-session snapshot was already taken,
// before the new backend is registered), the new backend must NOT end up
// in s.sessions after Shutdown completed. Either CreateSession returns
// an error, or the registry is empty post-shutdown — both prove no leak.
//
// Without the fix this test fails: CreateSession unblocks after Shutdown
// returns, writes the backend into s.sessions, and the host now reports
// a registered session against an already-shut-down BackendManager.
func TestService_ShutdownDuringCreateSession_DoesNotLeakBackend(t *testing.T) {
	t.Parallel()

	mgr := newSlowBackendManager()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: mgr,
		},
	})

	dir := initGitRepo(t, "git@github.com:acksell/clank.git")
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: dir},
		Prompt:  "race",
	}

	type result struct {
		backend agent.SessionBackend
		err     error
	}
	resCh := make(chan result, 1)
	go func() {
		b, _, err := svc.CreateSession(context.Background(), "sid-race", req)
		resCh <- result{backend: b, err: err}
	}()

	// Wait until CreateSession is blocked inside mgr.CreateBackend.
	select {
	case <-mgr.createEntry:
	case <-time.After(2 * time.Second):
		t.Fatal("CreateBackend was never reached")
	}

	// Shutdown races in. Live-session snapshot at this point is empty;
	// without the fix, Shutdown completes and then CreateSession writes
	// the backend into s.sessions post-shutdown.
	svc.Shutdown()
	if !mgr.wasShutdown() {
		t.Fatal("Shutdown did not call mgr.Shutdown()")
	}

	// Unblock CreateBackend and let CreateSession finish.
	close(mgr.release)

	var res result
	select {
	case res = <-resCh:
	case <-time.After(2 * time.Second):
		t.Fatal("CreateSession did not return after release")
	}

	// The contract: post-Shutdown, no leaked registration.
	// Acceptable outcomes:
	//   (a) CreateSession returned an error (host detected the shutdown
	//       and refused to register).
	//   (b) CreateSession succeeded but the backend was either Stop()'d
	//       and not registered, OR the registry is empty.
	if _, registered := svc.Session("sid-race"); registered {
		t.Fatalf("backend leaked into registry after Shutdown: err=%v", res.err)
	}
}
