package host_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/store"
)

// TestBusySessionCount pins the exit keepalive's don't-kill veto
// source: sessions mid-turn (busy/starting) count; settled states
// (idle/error/dead) don't.
func TestBusySessionCount(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
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

	if got := svc.BusySessionCount(ctx); got != 0 {
		t.Fatalf("empty store: BusySessionCount = %d, want 0", got)
	}

	seed := []agent.SessionInfo{
		{ID: "s-busy", Status: agent.StatusBusy},
		{ID: "s-starting", Status: agent.StatusStarting},
		{ID: "s-idle", Status: agent.StatusIdle},
		{ID: "s-error", Status: agent.StatusError},
		{ID: "s-dead", Status: agent.StatusDead},
	}
	for _, info := range seed {
		if err := st.UpsertSession(ctx, info); err != nil {
			t.Fatalf("UpsertSession(%s): %v", info.ID, err)
		}
	}
	if got := svc.BusySessionCount(ctx); got != 2 {
		t.Fatalf("BusySessionCount = %d, want 2 (busy + starting)", got)
	}
}
