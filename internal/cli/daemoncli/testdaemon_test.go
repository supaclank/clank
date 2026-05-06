package daemoncli

// Shared end-to-end test scaffolding: mounts a real host service behind
// a real gateway and gives the test a real daemonclient.Client. Replaces
// the bulk of the testing surface deleted with the hub package — every
// test that drove the hub's Unix-socket wire now drives the same routes
// through gateway → host.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/gateway"
	"github.com/acksell/clank/internal/host"
	hostmux "github.com/acksell/clank/internal/host/mux"
	hoststore "github.com/acksell/clank/internal/host/store"
)

// testDaemon is the assembled stack one test owns: a daemonclient
// pointing at a gateway, the in-process host service the gateway
// proxies to, and the stub backend manager so tests can inject events
// or observe what the picker dispatched.
type testDaemon struct {
	Client  *daemonclient.Client
	Service *host.Service
	Backend *stubBackendManager
	Store   *hoststore.Store
	HostURL string
	DBPath  string
}

// newTestDaemon spins up a fresh gateway + host. Cleanup is wired via
// t.Cleanup so callers don't have to defer.
func newTestDaemon(t *testing.T) *testDaemon {
	t.Helper()
	return newTestDaemonAt(t, filepath.Join(t.TempDir(), "host.db"))
}

// newTestDaemonAt is newTestDaemon but at a caller-provided dbPath.
// Used by restart-simulation tests that build a second daemon pointing
// at the same SQLite file as a torn-down first one.
func newTestDaemonAt(t *testing.T, dbPath string) *testDaemon {
	t.Helper()

	stub := &stubBackendManager{}

	hs, err := hoststore.Open(dbPath)
	if err != nil {
		t.Fatalf("hoststore.Open: %v", err)
	}
	t.Cleanup(func() { hs.Close() })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode:   stub,
			agent.BackendClaudeCode: stub,
		},
		ClonesDir:     t.TempDir(),
		SessionsStore: hs,
	})
	t.Cleanup(svc.Shutdown)

	hostHTTP := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(hostHTTP.Close)

	gw, err := gateway.NewGateway(gateway.Config{
		Provisioner: &fixedHostProvisioner{
			url:       hostHTTP.URL,
			transport: http.DefaultTransport,
		},
		Auth:          gateway.PermissiveAuth{},
		ResolveUserID: func(*http.Request) string { return "local" },
	}, nil)
	if err != nil {
		t.Fatalf("gateway.NewGateway: %v", err)
	}
	gwSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(gwSrv.Close)

	return &testDaemon{
		Client:  daemonclient.NewTCPClient(gwSrv.URL, ""),
		Service: svc,
		Backend: stub,
		Store:   hs,
		HostURL: gwSrv.URL,
		DBPath:  dbPath,
	}
}

// CreateOpenCodeSession is the one-liner most tests need: creates a
// session through the daemonclient using a temp git repo. Returns the
// SessionInfo and the just-attached stubBackend so the test can drive
// it directly (push events, observe Send calls, etc).
func (td *testDaemon) CreateOpenCodeSession(t *testing.T, prompt string) (*agent.SessionInfo, *stubBackend) {
	t.Helper()
	repo := initTestGitRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := td.Client.Sessions().Create(ctx, agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{LocalPath: repo, RemoteURL: "git@example.com:acme/repo.git"},
		Prompt:  prompt,
	})
	if err != nil {
		t.Fatalf("Sessions().Create: %v", err)
	}
	b := td.Backend.last
	if b == nil {
		t.Fatal("backend not registered after Create")
	}
	return info, b
}

// receiveEvents drains up to n events from ch (or until timeout fires).
// Returns what it managed to collect; callers assert on the slice.
// Mirrors the helper from the deleted hub event-roundtrip tests.
func receiveEvents(t *testing.T, ch <-chan agent.Event, n int, timeout time.Duration) []agent.Event {
	t.Helper()
	out := make([]agent.Event, 0, n)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline.C:
			return out
		}
	}
	return out
}

// receiveEventsByType drains ch until an event of `want` is seen or
// timeout fires. Returns the matched event (or nil) plus everything
// drained so the caller can assert ordering.
func receiveEventsByType(t *testing.T, ch <-chan agent.Event, want agent.EventType, timeout time.Duration) (*agent.Event, []agent.Event) {
	t.Helper()
	all := make([]agent.Event, 0, 8)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil, all
			}
			all = append(all, ev)
			if ev.Type == want {
				ret := ev
				return &ret, all
			}
		case <-deadline.C:
			return nil, all
		}
	}
}
