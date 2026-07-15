package host_test

import (
	"context"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// statusBackend is a noopBackend fixture whose Status() is fixed at
// construction, so tests can register real sessions with a chosen
// status via CreateSession instead of seeding the database directly.
type statusBackend struct {
	noopBackend
	status agent.SessionStatus
}

func (b *statusBackend) Status() agent.SessionStatus { return b.status }

type statusBackendManager struct {
	nextStatus agent.SessionStatus
}

func (m *statusBackendManager) Init(_ context.Context, _ func() ([]string, error)) error { return nil }
func (m *statusBackendManager) CreateBackend(_ context.Context, _ agent.BackendInvocation) (agent.SessionBackend, error) {
	return &statusBackend{status: m.nextStatus}, nil
}
func (m *statusBackendManager) Shutdown() {}

// TestBusySessionCount pins the exit keepalive's don't-kill veto
// source: sessions mid-turn (busy/starting) count; settled states
// (idle/error/dead) don't.
func TestBusySessionCount(t *testing.T) {
	t.Parallel()
	mgr := &statusBackendManager{}
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: mgr,
		},
	})
	t.Cleanup(svc.Shutdown)
	ctx := context.Background()

	if got := svc.BusySessionCount(); got != 0 {
		t.Fatalf("no sessions: BusySessionCount = %d, want 0", got)
	}

	dir := initGitRepo(t, "git@github.com:acksell/clank.git")
	seed := []struct {
		id     string
		status agent.SessionStatus
	}{
		{"s-busy", agent.StatusBusy},
		{"s-starting", agent.StatusStarting},
		{"s-idle", agent.StatusIdle},
		{"s-error", agent.StatusError},
		{"s-dead", agent.StatusDead},
	}
	for _, sd := range seed {
		mgr.nextStatus = sd.status
		req := agent.StartRequest{
			Backend: agent.BackendOpenCode,
			GitRef:  agent.GitRef{LocalPath: dir},
			Prompt:  "hi",
		}
		if _, _, err := svc.CreateSession(ctx, sd.id, req); err != nil {
			t.Fatalf("CreateSession(%s): %v", sd.id, err)
		}
	}
	if got := svc.BusySessionCount(); got != 2 {
		t.Fatalf("BusySessionCount = %d, want 2 (busy + starting)", got)
	}
}
