package clankcli

import (
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/pkg/sessionsync"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// TestSyncedSessionsRecord pins the export→record conversion: keyed by
// ExternalID, carrying the same Backend + UpdatedAt that went into the
// uploaded manifest.
func TestSyncedSessionsRecord(t *testing.T) {
	t.Parallel()
	exported := []sessionsync.ExportedSession{
		{Entry: checkpoint.SessionEntry{ExternalID: "x", Backend: agent.BackendOpenCode, UpdatedAt: time.UnixMilli(5000)}},
		{Entry: checkpoint.SessionEntry{ExternalID: "y", Backend: agent.BackendOpenCode, UpdatedAt: time.UnixMilli(6000)}},
	}
	rec := syncedSessionsRecord(exported)
	if len(rec.Sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(rec.Sessions))
	}
	x := rec.Sessions["x"]
	if x.Backend != agent.BackendOpenCode || !x.UpdatedAt.Equal(time.UnixMilli(5000)) {
		t.Errorf("x not converted faithfully: %+v", x)
	}
}
