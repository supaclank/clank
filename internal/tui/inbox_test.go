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
	groupForSession := make(map[string]string)
	for _, g := range m.groups {
		for _, r := range g.rows {
			ids = append(ids, r.session.ID)
			groupForSession[r.session.ID] = g.name
		}
	}

	// Archived sessions should not appear.
	for _, id := range ids {
		if id == "archived-1" {
			t.Errorf("archived session %q should not appear in groups", id)
		}
	}
	// Done sessions should appear in the DONE group at the bottom.
	if g, ok := groupForSession["done-1"]; !ok {
		t.Error("done session done-1 should appear in DONE group")
	} else if !strings.HasPrefix(g, "DONE") {
		t.Errorf("done-1: expected DONE group, got %q", g)
	}
	// visible-1, visible-2, and done-1 should all be present.
	if len(ids) != 3 {
		t.Errorf("expected 3 sessions, got %d: %v", len(ids), ids)
	}
	// DONE group should be last.
	lastGroup := m.groups[len(m.groups)-1]
	if !strings.HasPrefix(lastGroup.name, "DONE") {
		t.Errorf("expected DONE to be the last group, got %q", lastGroup.name)
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

	// The done session should appear in a DONE group; archived should be hidden.
	var totalRows int
	for _, g := range m.groups {
		totalRows += len(g.rows)
	}
	if totalRows != 1 {
		t.Errorf("expected 1 row (done session visible, archived hidden), got %d", totalRows)
	}
	if len(m.groups) != 1 || !strings.HasPrefix(m.groups[0].name, "DONE") {
		names := make([]string, len(m.groups))
		for i, g := range m.groups {
			names[i] = g.name
		}
		t.Errorf("expected single DONE group, got groups: %v", names)
	}
}

func TestRenderRow_DraftLabelShown(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{width: 120}

	session := &agent.SessionInfo{
		ID:          "draft-session",
		Status:      agent.StatusIdle,
		ProjectName: "myproject",
		Prompt:      "work in progress",
		UpdatedAt:   now,
		LastReadAt:  now,
		Draft:       "some unsent text",
	}
	row := inboxRow{session: session}
	rendered := m.renderRow(row, false)

	if !strings.Contains(rendered, "DRAFT") {
		t.Errorf("expected row to contain DRAFT label, got: %s", rendered)
	}
}

func TestRenderRow_DraftLabelPriorityOverUnreadAndFollowUp(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{width: 120}

	tests := []struct {
		name      string
		session   *agent.SessionInfo
		wantDRAFT bool
		wantBang  bool
		wantStar  bool
	}{
		{
			name: "draft takes priority over unread",
			session: &agent.SessionInfo{
				ID:          "draft-unread",
				Status:      agent.StatusIdle,
				ProjectName: "proj",
				Prompt:      "test",
				UpdatedAt:   now,
				// LastReadAt zero => Unread() is true
				Draft: "half-typed message",
			},
			wantDRAFT: true,
			wantBang:  false,
			wantStar:  false,
		},
		{
			name: "draft takes priority over follow-up",
			session: &agent.SessionInfo{
				ID:          "draft-followup",
				Status:      agent.StatusIdle,
				ProjectName: "proj",
				Prompt:      "test",
				UpdatedAt:   now,
				LastReadAt:  now,
				FollowUp:    true,
				Draft:       "half-typed message",
			},
			wantDRAFT: true,
			wantBang:  false,
			wantStar:  false,
		},
		{
			name: "no draft shows unread star",
			session: &agent.SessionInfo{
				ID:          "no-draft-unread",
				Status:      agent.StatusIdle,
				ProjectName: "proj",
				Prompt:      "test",
				UpdatedAt:   now,
				// LastReadAt zero => Unread() is true
			},
			wantDRAFT: false,
			wantBang:  false,
			wantStar:  true,
		},
		{
			name: "no draft shows follow-up bang",
			session: &agent.SessionInfo{
				ID:          "no-draft-followup",
				Status:      agent.StatusIdle,
				ProjectName: "proj",
				Prompt:      "test",
				UpdatedAt:   now,
				LastReadAt:  now,
				FollowUp:    true,
			},
			wantDRAFT: false,
			wantBang:  true,
			wantStar:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			row := inboxRow{session: tt.session}
			rendered := m.renderRow(row, false)

			hasDraft := strings.Contains(rendered, "DRAFT")
			// Check for isolated "!" — the follow-up mark is a standalone bold "!".
			hasBang := strings.Contains(rendered, "!")
			hasStar := strings.Contains(rendered, "*")

			if hasDraft != tt.wantDRAFT {
				t.Errorf("DRAFT: got %v, want %v; rendered: %s", hasDraft, tt.wantDRAFT, rendered)
			}
			if hasBang != tt.wantBang {
				t.Errorf("!: got %v, want %v; rendered: %s", hasBang, tt.wantBang, rendered)
			}
			if hasStar != tt.wantStar {
				t.Errorf("*: got %v, want %v; rendered: %s", hasStar, tt.wantStar, rendered)
			}
		})
	}
}

func TestBuildGroups_DraftSessionStaysInNormalGroup(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "idle-with-draft", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now, Draft: "wip text"},
		{ID: "idle-no-draft", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
		{ID: "busy-with-draft", Status: agent.StatusBusy, UpdatedAt: now.Add(-30 * time.Minute), Draft: "busy draft"},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// There should be no DRAFT group — sessions stay in their normal groups.
	for _, g := range m.groups {
		if strings.Contains(g.name, "DRAFT") {
			t.Errorf("unexpected DRAFT group: %q; drafts should stay in their normal group", g.name)
		}
	}

	// Verify the draft sessions are in the expected groups.
	groupForSession := make(map[string]string)
	for _, g := range m.groups {
		for _, r := range g.rows {
			groupForSession[r.session.ID] = g.name
		}
	}

	if g, ok := groupForSession["idle-with-draft"]; !ok {
		t.Error("idle-with-draft not found in any group")
	} else if !strings.HasPrefix(g, "IDLE") {
		t.Errorf("idle-with-draft: expected IDLE group, got %q", g)
	}

	if g, ok := groupForSession["busy-with-draft"]; !ok {
		t.Error("busy-with-draft not found in any group")
	} else if !strings.HasPrefix(g, "BUSY") {
		t.Errorf("busy-with-draft: expected BUSY group, got %q", g)
	}
}

func TestBuildGroups_FollowUpToggleMovesSessionBetweenGroups(t *testing.T) {
	t.Parallel()

	now := time.Now()

	// Start with a session that is NOT follow-up — it should be in the IDLE group.
	sessions := []agent.SessionInfo{
		{ID: "s1", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now, FollowUp: false},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}
	if !strings.HasPrefix(m.groups[0].name, "IDLE") {
		t.Fatalf("expected IDLE group, got %q", m.groups[0].name)
	}

	// Toggle follow-up on — session should move to the FOLLOW UP group.
	sessions[0].FollowUp = true
	m.buildGroups(sessions)

	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group after follow-up toggle, got %d", len(m.groups))
	}
	if !strings.HasPrefix(m.groups[0].name, "FOLLOW UP") {
		t.Fatalf("expected FOLLOW UP group, got %q", m.groups[0].name)
	}

	// Toggle follow-up off — session should move back to the IDLE group.
	sessions[0].FollowUp = false
	m.buildGroups(sessions)

	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group after follow-up untoggle, got %d", len(m.groups))
	}
	if !strings.HasPrefix(m.groups[0].name, "IDLE") {
		t.Fatalf("expected IDLE group after untoggle, got %q", m.groups[0].name)
	}
}

func TestBuildGroups_FollowUpBusySessionStaysInBusy(t *testing.T) {
	t.Parallel()

	now := time.Now()

	// A busy session with follow-up should stay in the BUSY group, not FOLLOW UP.
	sessions := []agent.SessionInfo{
		{ID: "busy-fu", Status: agent.StatusBusy, UpdatedAt: now, FollowUp: true},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}
	if !strings.HasPrefix(m.groups[0].name, "BUSY") {
		t.Errorf("expected BUSY group for busy+follow-up session, got %q", m.groups[0].name)
	}
}

func TestCategoryNavigation(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// 3 groups: BUSY (2 rows), FOLLOW UP (3 rows), IDLE (2 rows)
	// flatRows indices: 0,1 = BUSY; 2,3,4 = FOLLOW UP; 5,6 = IDLE
	sessions := []agent.SessionInfo{
		{ID: "busy-1", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "busy-2", Status: agent.StatusBusy, UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "fu-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now, FollowUp: true},
		{ID: "fu-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now, FollowUp: true},
		{ID: "fu-3", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now, FollowUp: true},
		{ID: "idle-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
		{ID: "idle-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
	}

	setup := func() *InboxModel {
		m := &InboxModel{}
		m.buildGroups(sessions)
		return m
	}

	t.Run("cursorGroupIndex", func(t *testing.T) {
		t.Parallel()
		m := setup()

		tests := []struct {
			cursor    int
			wantGroup int
		}{
			{0, 0}, {1, 0}, // BUSY
			{2, 1}, {3, 1}, {4, 1}, // FOLLOW UP
			{5, 2}, {6, 2}, // IDLE
		}
		for _, tt := range tests {
			m.cursor = tt.cursor
			got := m.cursorGroupIndex()
			if got != tt.wantGroup {
				t.Errorf("cursor=%d: cursorGroupIndex()=%d, want %d", tt.cursor, got, tt.wantGroup)
			}
		}
	})

	t.Run("groupFirstRow", func(t *testing.T) {
		t.Parallel()
		m := setup()

		tests := []struct {
			groupIdx int
			wantRow  int
		}{
			{0, 0}, // BUSY starts at 0
			{1, 2}, // FOLLOW UP starts at 2
			{2, 5}, // IDLE starts at 5
		}
		for _, tt := range tests {
			got := m.groupFirstRow(tt.groupIdx)
			if got != tt.wantRow {
				t.Errorf("groupFirstRow(%d)=%d, want %d", tt.groupIdx, got, tt.wantRow)
			}
		}
	})

	t.Run("cmd+up from middle of category goes to top of that category", func(t *testing.T) {
		t.Parallel()
		m := setup()

		// Cursor at fu-2 (index 3, middle of FOLLOW UP group)
		m.cursor = 3
		gi := m.cursorGroupIndex()
		first := m.groupFirstRow(gi)
		if m.cursor != first {
			m.cursor = first
		}
		if m.cursor != 2 {
			t.Errorf("cursor=%d, want 2 (top of FOLLOW UP)", m.cursor)
		}
	})

	t.Run("cmd+up from top of category goes to top of category above", func(t *testing.T) {
		t.Parallel()
		m := setup()

		// Cursor at fu-1 (index 2, top of FOLLOW UP)
		m.cursor = 2
		gi := m.cursorGroupIndex()
		first := m.groupFirstRow(gi)
		if m.cursor == first && gi > 0 {
			m.cursor = m.groupFirstRow(gi - 1)
		}
		if m.cursor != 0 {
			t.Errorf("cursor=%d, want 0 (top of BUSY)", m.cursor)
		}
	})

	t.Run("cmd+up from top of first category stays put", func(t *testing.T) {
		t.Parallel()
		m := setup()

		m.cursor = 0
		gi := m.cursorGroupIndex()
		first := m.groupFirstRow(gi)
		if m.cursor == first && gi > 0 {
			m.cursor = m.groupFirstRow(gi - 1)
		} else if m.cursor != first {
			m.cursor = first
		}
		if m.cursor != 0 {
			t.Errorf("cursor=%d, want 0 (should stay at top of first category)", m.cursor)
		}
	})

	t.Run("cmd+down goes to top of next category", func(t *testing.T) {
		t.Parallel()
		m := setup()

		// From middle of BUSY (index 1)
		m.cursor = 1
		gi := m.cursorGroupIndex()
		if gi < len(m.groups)-1 {
			m.cursor = m.groupFirstRow(gi + 1)
		}
		if m.cursor != 2 {
			t.Errorf("cursor=%d, want 2 (top of FOLLOW UP)", m.cursor)
		}

		// From top of FOLLOW UP (index 2) to top of IDLE
		gi = m.cursorGroupIndex()
		if gi < len(m.groups)-1 {
			m.cursor = m.groupFirstRow(gi + 1)
		}
		if m.cursor != 5 {
			t.Errorf("cursor=%d, want 5 (top of IDLE)", m.cursor)
		}
	})

	t.Run("cmd+down from last category goes to very last row", func(t *testing.T) {
		t.Parallel()
		m := setup()

		// Cursor at idle-1 (index 5, top of last group IDLE)
		m.cursor = 5
		gi := m.cursorGroupIndex()
		if gi < len(m.groups)-1 {
			m.cursor = m.groupFirstRow(gi + 1)
		} else {
			m.cursor = len(m.flatRows) - 1
		}
		if m.cursor != 6 {
			t.Errorf("cursor=%d, want 6 (last row of inbox)", m.cursor)
		}
	})

	t.Run("cmd+down from very last row stays put", func(t *testing.T) {
		t.Parallel()
		m := setup()

		// Cursor already at the very last row
		m.cursor = 6
		gi := m.cursorGroupIndex()
		if gi < len(m.groups)-1 {
			m.cursor = m.groupFirstRow(gi + 1)
		} else {
			m.cursor = len(m.flatRows) - 1
		}
		if m.cursor != 6 {
			t.Errorf("cursor=%d, want 6 (should stay at last row)", m.cursor)
		}
	})
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
