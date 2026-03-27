package tui

import (
	"strings"
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

func TestBuildGroups_FiltersHiddenSessions(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "visible-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
		{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityDone},
		{ID: "archived-1", Status: agent.StatusBusy, UpdatedAt: now.Add(-30 * time.Minute), Visibility: agent.VisibilityArchived},
		{ID: "visible-2", Status: agent.StatusBusy, UpdatedAt: now.Add(-15 * time.Minute)},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// Collect all session IDs across all groups.
	var ids []string
	for _, g := range m.groups {
		for _, r := range g.rows {
			ids = append(ids, r.session.ID)
		}
	}

	// Only visible sessions should be present.
	for _, id := range ids {
		if id == "done-1" || id == "archived-1" {
			t.Errorf("hidden session %q should not appear in groups", id)
		}
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 visible sessions, got %d: %v", len(ids), ids)
	}
}

func TestBuildGroups_AllHiddenResultsInEmptyGroups(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now, Visibility: agent.VisibilityDone},
		{ID: "archived-1", Status: agent.StatusIdle, UpdatedAt: now, Visibility: agent.VisibilityArchived},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// All sessions hidden — no groups should be created.
	var totalRows int
	for _, g := range m.groups {
		totalRows += len(g.rows)
	}
	if totalRows != 0 {
		t.Errorf("expected 0 rows when all sessions are hidden, got %d", totalRows)
	}
}

func TestRenderRow_ShowsAgentMode(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{width: 120}

	tests := []struct {
		name      string
		agent     string
		wantInRow string // substring expected in rendered output
	}{
		{name: "build agent shown", agent: "build", wantInRow: "build"},
		{name: "plan agent shown", agent: "plan", wantInRow: "plan"},
		{name: "empty agent shows blank", agent: "", wantInRow: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			session := &agent.SessionInfo{
				ID:          "test-session",
				Status:      agent.StatusIdle,
				ProjectName: "myproject",
				Prompt:      "do something",
				Agent:       tt.agent,
				UpdatedAt:   now,
				LastReadAt:  now,
			}
			row := inboxRow{session: session}
			rendered := m.renderRow(row, false)

			if tt.wantInRow != "" && !strings.Contains(rendered, tt.wantInRow) {
				t.Errorf("expected row to contain %q, got: %s", tt.wantInRow, rendered)
			}
			// When agent is empty, verify neither "build" nor "plan" appears.
			if tt.agent == "" {
				if strings.Contains(rendered, "build") || strings.Contains(rendered, "plan") {
					t.Errorf("expected no agent name in row, got: %s", rendered)
				}
			}
		})
	}
}
