package host_test

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/internal/host/store"
)

// fakeDiscovererBackend is a noop backend that implements
// agent.SessionDiscoverer so DiscoverSessions reaches persistSnapshots.
type fakeDiscovererBackend struct {
	noopBackendManager
	snapshots []agent.SessionSnapshot
}

func (f *fakeDiscovererBackend) DiscoverSessions(_ context.Context, _ string) ([]agent.SessionSnapshot, error) {
	return f.snapshots, nil
}

// TestPersistSnapshotsSkipsOnNonNotFoundLookupError is a regression test:
// when the sessions store returns a non-NotFound error from
// FindSessionByExternalID (e.g. the underlying DB has been closed),
// persistSnapshots must NOT fall through to UpsertSession. Previously,
// every non-nil error was treated as "not registered" and a stale lookup
// failure would compound into an unrelated insert attempt.
func TestPersistSnapshotsSkipsOnNonNotFoundLookupError(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	var logBuf bytes.Buffer
	svc := host.New(host.Options{
		ID: "test-host",
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &fakeDiscovererBackend{
				snapshots: []agent.SessionSnapshot{
					{
						ID:        "ext-A",
						Backend:   agent.BackendOpenCode,
						Title:     "fake",
						Directory: t.TempDir(),
						CreatedAt: time.Now(),
						UpdatedAt: time.Now(),
					},
				},
			},
		},
		ClonesDir:     t.TempDir(),
		SessionsStore: st,
		Log:           log.New(&logBuf, "", 0),
	})
	t.Cleanup(svc.Shutdown)

	// Closing the underlying store causes FindSessionByExternalID to
	// fail with a non-NotFound error (database is closed).
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	_, _ = svc.DiscoverSessions(context.Background(), agent.BackendOpenCode, t.TempDir())

	logs := logBuf.String()
	if !strings.Contains(logs, "lookup snapshot extID=ext-A") {
		t.Errorf("expected lookup-error log line for ext-A; got logs:\n%s", logs)
	}
	if strings.Contains(logs, "persist snapshot extID=ext-A") {
		t.Errorf("UpsertSession was called despite lookup error; got logs:\n%s", logs)
	}

	// Reopen the store and confirm no session was persisted.
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	got, err := st2.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no sessions persisted on lookup error; got %d", len(got))
	}
}
