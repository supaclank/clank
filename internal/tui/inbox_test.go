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

	// Archived sessions should appear in the ARCHIVED group.
	if g, ok := groupForSession["archived-1"]; !ok {
		t.Error("archived session archived-1 should appear in ARCHIVED group")
	} else if !strings.HasPrefix(g, "ARCHIVED") {
		t.Errorf("archived-1: expected ARCHIVED group, got %q", g)
	}
	// Done sessions should appear in the DONE group.
	if g, ok := groupForSession["done-1"]; !ok {
		t.Error("done session done-1 should appear in DONE group")
	} else if !strings.HasPrefix(g, "DONE") {
		t.Errorf("done-1: expected DONE group, got %q", g)
	}
	// All four sessions should be present.
	if len(ids) != 4 {
		t.Errorf("expected 4 sessions, got %d: %v", len(ids), ids)
	}
	// ARCHIVED group should be last.
	lastGroup := m.groups[len(m.groups)-1]
	if !strings.HasPrefix(lastGroup.name, "ARCHIVED") {
		t.Errorf("expected ARCHIVED to be the last group, got %q", lastGroup.name)
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

	// Both done and archived sessions should appear in their own groups.
	var totalRows int
	for _, g := range m.groups {
		totalRows += len(g.rows)
	}
	if totalRows != 2 {
		t.Errorf("expected 2 rows (done + archived), got %d", totalRows)
	}
	if len(m.groups) != 2 {
		names := make([]string, len(m.groups))
		for i, g := range m.groups {
			names[i] = g.name
		}
		t.Errorf("expected DONE and ARCHIVED groups, got groups: %v", names)
	}
	if !strings.HasPrefix(m.groups[0].name, "DONE") {
		t.Errorf("expected first group to be DONE, got %q", m.groups[0].name)
	}
	if !strings.HasPrefix(m.groups[1].name, "ARCHIVED") {
		t.Errorf("expected second group to be ARCHIVED, got %q", m.groups[1].name)
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

// setupScrollModel creates an InboxModel with the given number of idle sessions
// and a viewport height of vh (sets m.height = vh + 4 to account for reserved lines).
// It builds groups and display lines so ensureCursorVisible can be tested directly.
func setupScrollModel(t *testing.T, numSessions int, vh int) *InboxModel {
	t.Helper()

	now := time.Now()
	sessions := make([]agent.SessionInfo, numSessions)
	for i := range sessions {
		sessions[i] = agent.SessionInfo{
			ID:         "s" + time.Duration(i).String(),
			Status:     agent.StatusIdle,
			UpdatedAt:  now.Add(-time.Duration(i) * time.Hour),
			LastReadAt: now,
		}
	}

	m := &InboxModel{
		height: vh + 4, // viewportHeight() = height - 4
		width:  120,
	}
	m.buildGroups(sessions)
	m.buildDisplayLines()
	return m
}

func TestEnsureCursorVisible_ScrollsDownWithMargin(t *testing.T) {
	t.Parallel()

	// vh=30, margin = 30*10/100 = 3.
	// 40 sessions in one IDLE group => display lines: 1 header + 40 rows = 41 lines.
	// rowToLine[0]=1, rowToLine[1]=2, ..., rowToLine[i]=i+1.
	m := setupScrollModel(t, 40, 30)
	m.scrollOffset = 0
	m.cursor = 0

	// Walk the cursor down one-by-one and verify scrolling behavior.
	for i := 0; i < len(m.flatRows); i++ {
		m.cursor = i
		m.ensureCursorVisible()

		cursorLine := m.rowToLine[m.cursor]
		vh := m.viewportHeight()
		margin := vh * 10 / 100
		if margin < 2 {
			margin = 2
		}

		// Cursor should never be in the bottom margin zone (unless clamped at the end).
		maxOffset := len(m.displayLines) - vh
		if maxOffset < 0 {
			maxOffset = 0
		}
		if m.scrollOffset < maxOffset {
			bottomEdge := m.scrollOffset + vh - margin
			if cursorLine >= bottomEdge {
				t.Errorf("cursor=%d cursorLine=%d is in the bottom margin (scrollOffset=%d, vh=%d, margin=%d, bottomEdge=%d)",
					m.cursor, cursorLine, m.scrollOffset, vh, margin, bottomEdge)
			}
		}
	}
}

func TestEnsureCursorVisible_ScrollsUpWithMargin(t *testing.T) {
	t.Parallel()

	// vh=30, margin=3, 40 sessions.
	m := setupScrollModel(t, 40, 30)

	// Start with cursor at the bottom.
	m.cursor = len(m.flatRows) - 1
	m.ensureCursorVisible()

	// Walk the cursor up one-by-one.
	for i := len(m.flatRows) - 1; i >= 0; i-- {
		m.cursor = i
		m.ensureCursorVisible()

		cursorLine := m.rowToLine[m.cursor]
		vh := m.viewportHeight()
		margin := vh * 10 / 100
		if margin < 2 {
			margin = 2
		}

		// Cursor should never be in the top margin zone (unless clamped at offset 0).
		if m.scrollOffset > 0 {
			topEdge := m.scrollOffset + margin
			if cursorLine < topEdge {
				t.Errorf("cursor=%d cursorLine=%d is in the top margin (scrollOffset=%d, margin=%d, topEdge=%d)",
					m.cursor, cursorLine, m.scrollOffset, margin, topEdge)
			}
		}
	}
}

func TestEnsureCursorVisible_CursorNeverOutsideViewport(t *testing.T) {
	t.Parallel()

	m := setupScrollModel(t, 50, 20)

	// Sweep cursor through every position.
	for i := 0; i < len(m.flatRows); i++ {
		m.cursor = i
		m.ensureCursorVisible()

		cursorLine := m.rowToLine[m.cursor]
		vh := m.viewportHeight()

		if cursorLine < m.scrollOffset {
			t.Errorf("cursor=%d cursorLine=%d above viewport (scrollOffset=%d)", m.cursor, cursorLine, m.scrollOffset)
		}
		if cursorLine >= m.scrollOffset+vh {
			t.Errorf("cursor=%d cursorLine=%d below viewport (scrollOffset=%d, vh=%d)", m.cursor, cursorLine, m.scrollOffset, vh)
		}
	}
}

func TestEnsureCursorVisible_SmallViewportMinMargin(t *testing.T) {
	t.Parallel()

	// vh=10, margin = 10*10/100 = 1, but min is 2. So margin=2.
	m := setupScrollModel(t, 20, 10)
	m.cursor = 0
	m.scrollOffset = 0

	for i := 0; i < len(m.flatRows); i++ {
		m.cursor = i
		m.ensureCursorVisible()

		cursorLine := m.rowToLine[m.cursor]
		vh := m.viewportHeight()
		margin := 2 // min margin

		maxOffset := len(m.displayLines) - vh
		if maxOffset < 0 {
			maxOffset = 0
		}
		if m.scrollOffset > 0 && m.scrollOffset < maxOffset {
			if cursorLine < m.scrollOffset+margin {
				t.Errorf("cursor=%d cursorLine=%d too close to top (scrollOffset=%d, margin=%d)",
					m.cursor, cursorLine, m.scrollOffset, margin)
			}
			if cursorLine >= m.scrollOffset+vh-margin {
				t.Errorf("cursor=%d cursorLine=%d too close to bottom (scrollOffset=%d, vh=%d, margin=%d)",
					m.cursor, cursorLine, m.scrollOffset, vh, margin)
			}
		}
	}
}

func TestEnsureCursorVisible_CategoryBoundaryNoJump(t *testing.T) {
	t.Parallel()

	// Create a model with 2 groups: a large BUSY group and a large IDLE group.
	// This tests that crossing the category boundary doesn't cause a large jump.
	now := time.Now()
	sessions := make([]agent.SessionInfo, 0, 40)
	for i := 0; i < 20; i++ {
		sessions = append(sessions, agent.SessionInfo{
			ID:         "busy-" + time.Duration(i).String(),
			Status:     agent.StatusBusy,
			UpdatedAt:  now.Add(-time.Duration(i) * time.Hour),
			LastReadAt: now,
		})
	}
	for i := 0; i < 20; i++ {
		sessions = append(sessions, agent.SessionInfo{
			ID:         "idle-" + time.Duration(i).String(),
			Status:     agent.StatusIdle,
			UpdatedAt:  now.Add(-time.Duration(i) * time.Hour),
			LastReadAt: now,
		})
	}

	vh := 30
	m := &InboxModel{
		height: vh + 4,
		width:  120,
	}
	m.buildGroups(sessions)
	m.buildDisplayLines()

	// Position cursor in the IDLE group (second group), somewhere in the middle.
	// First group has 20 rows, so flatRows[20] is the first IDLE row.
	m.cursor = 25
	m.ensureCursorVisible()
	prevOffset := m.scrollOffset

	// Move cursor up across the category boundary (from IDLE into BUSY).
	for i := m.cursor; i >= 15; i-- {
		m.cursor = i
		m.ensureCursorVisible()

		// The scroll offset should change smoothly — never jumping more than
		// a few lines at a time for single-step cursor movements.
		delta := prevOffset - m.scrollOffset
		if delta < 0 {
			delta = -delta
		}
		// A single cursor move should shift the viewport by at most ~2 lines
		// (1 for the row + possibly 1 for a blank separator or header entering view).
		// Allow up to 3 to be safe.
		if delta > 3 {
			t.Errorf("cursor=%d: scrollOffset jumped by %d (from %d to %d) — expected smooth scrolling across category boundary",
				m.cursor, delta, prevOffset, m.scrollOffset)
		}
		prevOffset = m.scrollOffset
	}
}

func TestEnsureCursorVisible_EmptyList(t *testing.T) {
	t.Parallel()

	m := &InboxModel{height: 34, width: 120}
	m.buildGroups(nil)
	m.buildDisplayLines()
	m.ensureCursorVisible()

	if m.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0 for empty list, got %d", m.scrollOffset)
	}
}

func TestEnsureCursorVisible_ContentShorterThanViewport(t *testing.T) {
	t.Parallel()

	// Only 3 sessions with vh=30 — content fits entirely on screen.
	m := setupScrollModel(t, 3, 30)

	for i := 0; i < len(m.flatRows); i++ {
		m.cursor = i
		m.ensureCursorVisible()

		// scrollOffset should stay 0 since everything fits.
		if m.scrollOffset != 0 {
			t.Errorf("cursor=%d: expected scrollOffset=0 when content fits in viewport, got %d",
				m.cursor, m.scrollOffset)
		}
	}
}
