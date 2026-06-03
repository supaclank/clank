package sessionsync

import (
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

// TestWithinPushWindow pins the recency cut-off: a never-synced session older
// than the window is dropped, while a recent one and an already-synced one
// (regardless of age) are kept — and a zero cutoff keeps everything.
func TestWithinPushWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cutoff := now.Add(-SessionPushWindow)
	old := now.Add(-30 * 24 * time.Hour) // well past the window

	all := []DiscoveredSession{
		{ExternalID: "recent", UpdatedAt: now.Add(-time.Hour)},
		{ExternalID: "old-unsynced", UpdatedAt: old},
		{ExternalID: "old-synced", UpdatedAt: old},
	}
	// old-synced is already in the record, so age must not drop it.
	rec := agent.SyncedSessionRecord{Sessions: map[string]agent.SyncedSession{
		"old-synced": {ContentHash: "abc"},
	}}

	keptIDs := func(sessions []DiscoveredSession) map[string]bool {
		m := make(map[string]bool, len(sessions))
		for _, s := range sessions {
			m[s.ExternalID] = true
		}
		return m
	}

	// Default window: recent + previously-synced kept; old-never-synced dropped.
	got := keptIDs(WithinPushWindow(all, rec, cutoff))
	if !got["recent"] || !got["old-synced"] || got["old-unsynced"] {
		t.Fatalf("window filter wrong: kept=%v, want {recent, old-synced}", got)
	}

	// Zero cutoff (no window): everything kept, including old-never-synced.
	if all2 := WithinPushWindow(all, rec, time.Time{}); len(all2) != 3 {
		t.Fatalf("zero cutoff dropped sessions: kept %d, want 3", len(all2))
	}
}
