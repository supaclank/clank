package tui

import (
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
)

func TestBuildGroups_SortsByUpdatedAtDescending(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "busy-old", Status: agent.StatusBusy, UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "busy-new", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "idle-old", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
		{ID: "idle-new", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
		{ID: "unread-old", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "unread-new", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "error-old", Status: agent.StatusError, UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "error-new", Status: agent.StatusError, UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "dead-old", Status: agent.StatusDead, UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "dead-new", Status: agent.StatusDead, UpdatedAt: now.Add(-1 * time.Hour)},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// Verify group order: busy, unread, idle, error, dead.
	wantGroupOrder := []string{
		"BUSY (2)",
		"UNREAD (2)",
		"IDLE (2)",
		"ERROR (2)",
		"DEAD (2)",
	}
	if len(m.groups) != len(wantGroupOrder) {
		t.Fatalf("expected %d groups, got %d", len(wantGroupOrder), len(m.groups))
	}
	for i, g := range m.groups {
		if g.name != wantGroupOrder[i] {
			t.Errorf("group[%d]: got name %q, want %q", i, g.name, wantGroupOrder[i])
		}
	}

	// Within each group, the newer session (larger UpdatedAt) should come first.
	wantFirstIDs := []string{"busy-new", "unread-new", "idle-new", "error-new", "dead-new"}
	for i, g := range m.groups {
		if g.rows[0].session.ID != wantFirstIDs[i] {
			t.Errorf("group %q: first row ID = %q, want %q", g.name, g.rows[0].session.ID, wantFirstIDs[i])
		}
	}
}

func TestBuildGroups_StableAcrossRepeatedCalls(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "a", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
		{ID: "b", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
		{ID: "c", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
	}

	m := &InboxModel{}

	// Run buildGroups multiple times; order must be the same each time.
	for iter := 0; iter < 20; iter++ {
		m.buildGroups(sessions)
		if len(m.groups) != 1 {
			t.Fatalf("iter %d: expected 1 group, got %d", iter, len(m.groups))
		}
		ids := make([]string, len(m.groups[0].rows))
		for i, r := range m.groups[0].rows {
			ids[i] = r.session.ID
		}
		want := []string{"b", "c", "a"}
		for i, id := range ids {
			if id != want[i] {
				t.Fatalf("iter %d: row[%d] = %q, want %q (full order: %v)", iter, i, id, want[i], ids)
			}
		}
	}
}
