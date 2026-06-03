package host_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/store"
)

// TestInit_NormalizesStaleErrorToIdle pins that a session persisted as
// StatusError from a previous daemon run is reset to idle on startup. Without
// this, a transient failure — e.g. a session opened before its worktree
// finished materializing — strands the session permanently red with no
// recovery path (the failing backend that set the status is long gone).
func TestInit_NormalizesStaleErrorToIdle(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const id = "01HSTALEERROR0000000000000"
	seed := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := st.UpsertSession(context.Background(), agent.SessionInfo{
		ID: id, ExternalID: "ses_stale", Backend: agent.BackendOpenCode,
		Status: agent.StatusError, CreatedAt: seed, UpdatedAt: seed,
	}); err != nil {
		t.Fatal(err)
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{agent.BackendOpenCode: &noopBackendManager{}},
		SessionsStore:   st,
	})
	t.Cleanup(svc.Shutdown)
	if err := svc.Init(context.Background(), func(agent.BackendType) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}

	got, err := st.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agent.StatusIdle {
		t.Errorf("status = %q, want idle (a stale error must normalize on startup)", got.Status)
	}
	// The sweep must not hoist the session to the top of the inbox.
	if !got.UpdatedAt.Equal(seed) {
		t.Errorf("UpdatedAt moved to %v, want preserved at %v", got.UpdatedAt, seed)
	}
}
