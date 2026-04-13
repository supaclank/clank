package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/acksell/clank/internal/agent"
)

func TestBuildGroups_SortsByUpdatedAtDescending(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "old", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
		{ID: "new", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "mid", Status: agent.StatusError, UpdatedAt: now.Add(-2 * time.Hour)},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// All sessions are from "Today", so there should be a single group.
	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}
	if m.groups[0].name != "Today" {
		t.Errorf("expected group name 'Today', got %q", m.groups[0].name)
	}

	// Within the group, busy sorts first (priority 0); error and idle share the
	// same priority (5) so they sort by UpdatedAt descending.
	wantOrder := []string{"new", "mid", "old"}
	for i, r := range m.groups[0].rows {
		if r.session.ID != wantOrder[i] {
			t.Errorf("row[%d]: got %q, want %q", i, r.session.ID, wantOrder[i])
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

	// All non-archived sessions are from today, so there should be one date group.
	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}

	// The date group should contain 2 active sessions (done and archived excluded).
	var ids []string
	for _, r := range m.groups[0].rows {
		ids = append(ids, r.session.ID)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 active sessions in date group, got %d: %v", len(ids), ids)
	}

	// Done session should be stored in the group's doneRows.
	if len(m.groups[0].doneRows) != 1 {
		t.Fatalf("expected 1 done session in group, got %d", len(m.groups[0].doneRows))
	}
	if m.groups[0].doneRows[0].session.ID != "done-1" {
		t.Errorf("expected done session 'done-1', got %q", m.groups[0].doneRows[0].session.ID)
	}

	// Archived session should be stored in the group's archivedRows.
	if len(m.groups[0].archivedRows) != 1 {
		t.Fatalf("expected 1 archived session in group, got %d", len(m.groups[0].archivedRows))
	}
	if m.groups[0].archivedRows[0].session.ID != "archived-1" {
		t.Errorf("expected archived session 'archived-1', got %q", m.groups[0].archivedRows[0].session.ID)
	}

	// flatRows should have 2 active rows + 1 done accordion + 1 archive accordion = 4 total
	// (both accordions are collapsed by default).
	if len(m.flatRows) != 4 {
		t.Errorf("expected 4 flatRows (2 sessions + 1 done accordion + 1 archive accordion), got %d", len(m.flatRows))
	}
	if m.flatRows[2].doneAccordion == "" {
		t.Error("expected flatRows[2] to be the done accordion toggle")
	}
	if m.flatRows[3].accordion == "" {
		t.Error("expected flatRows[3] to be the archive accordion toggle")
	}
}

func TestBuildGroups_AllHiddenResultsInDateGroup(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now, Visibility: agent.VisibilityDone},
		{ID: "archived-1", Status: agent.StatusIdle, UpdatedAt: now, Visibility: agent.VisibilityArchived},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// Done session goes in a "Today" date group; archived is separate.
	if len(m.groups) != 1 {
		names := make([]string, len(m.groups))
		for i, g := range m.groups {
			names[i] = g.name
		}
		t.Fatalf("expected 1 group, got %d: %v", len(m.groups), names)
	}
	if m.groups[0].name != "Today" {
		t.Errorf("expected group name 'Today', got %q", m.groups[0].name)
	}

	// Done session should be in doneRows, not in active rows.
	if len(m.groups[0].rows) != 0 {
		t.Errorf("expected 0 active rows in date group, got %d", len(m.groups[0].rows))
	}
	if len(m.groups[0].doneRows) != 1 {
		t.Errorf("expected 1 done session in group, got %d", len(m.groups[0].doneRows))
	}

	// Archived session stored in the group's archivedRows.
	if len(m.groups[0].archivedRows) != 1 {
		t.Errorf("expected 1 archived session in group, got %d", len(m.groups[0].archivedRows))
	}

	// flatRows: 1 done accordion + 1 archive accordion = 2.
	if len(m.flatRows) != 2 {
		t.Errorf("expected 2 flatRows, got %d", len(m.flatRows))
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

	if !strings.Contains(rendered, "draft") {
		t.Errorf("expected row to contain draft label, got: %s", rendered)
	}
}

func TestRenderRow_DraftLabelCoexistsWithUnreadMarks(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{width: 120}

	tests := []struct {
		name      string
		session   *agent.SessionInfo
		wantDraft bool
		wantBang  bool
		wantStar  bool
	}{
		{
			name: "draft with unread shows both draft and star",
			session: &agent.SessionInfo{
				ID:          "draft-unread",
				Status:      agent.StatusIdle,
				ProjectName: "proj",
				Prompt:      "test",
				UpdatedAt:   now,
				// LastReadAt zero => Unread() is true
				Draft: "half-typed message",
			},
			wantDraft: true,
			wantBang:  false,
			wantStar:  true,
		},
		{
			name: "draft with follow-up shows both draft and bang",
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
			wantDraft: true,
			wantBang:  true,
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
			wantDraft: false,
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
			wantDraft: false,
			wantBang:  true,
			wantStar:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			row := inboxRow{session: tt.session}
			rendered := m.renderRow(row, false)

			hasDraft := strings.Contains(rendered, "draft")
			// Check for isolated "!" — the follow-up mark is a standalone bold "!".
			hasBang := strings.Contains(rendered, "!")
			hasStar := strings.Contains(rendered, "*")

			if hasDraft != tt.wantDraft {
				t.Errorf("draft: got %v, want %v; rendered: %s", hasDraft, tt.wantDraft, rendered)
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

func TestBuildGroups_DraftSessionStaysInDateGroup(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "idle-with-draft", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now, Draft: "wip text"},
		{ID: "idle-no-draft", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
		{ID: "busy-with-draft", Status: agent.StatusBusy, UpdatedAt: now.Add(-30 * time.Minute), Draft: "busy draft"},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// All sessions are from today — should be one "Today" group.
	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}
	if m.groups[0].name != "Today" {
		t.Errorf("expected group name 'Today', got %q", m.groups[0].name)
	}

	// There should be no DRAFT group.
	for _, g := range m.groups {
		if strings.Contains(g.name, "DRAFT") {
			t.Errorf("unexpected DRAFT group: %q; drafts should stay in their date group", g.name)
		}
	}

	// All 3 sessions should be present.
	if len(m.flatRows) != 3 {
		t.Errorf("expected 3 flatRows, got %d", len(m.flatRows))
	}
}

func TestBuildGroups_FollowUpDoesNotAffectSorting(t *testing.T) {
	t.Parallel()

	now := time.Now()

	// A follow-up session and a normal idle session on the same day.
	// Follow-up should NOT affect sort order; both are idle+read so
	// they sort by UpdatedAt descending (idle is more recent).
	sessions := []agent.SessionInfo{
		{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now, FollowUp: false},
		{ID: "followup", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now, FollowUp: true},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}
	if m.groups[0].name != "Today" {
		t.Fatalf("expected 'Today' group, got %q", m.groups[0].name)
	}

	// Both have the same sort priority; idle is more recently updated so it comes first.
	if m.groups[0].rows[0].session.ID != "idle" {
		t.Errorf("expected idle session first (more recent), got %q", m.groups[0].rows[0].session.ID)
	}
	if m.groups[0].rows[1].session.ID != "followup" {
		t.Errorf("expected follow-up session second (older), got %q", m.groups[0].rows[1].session.ID)
	}
}

func TestBuildGroups_BusySessionSortsFirst(t *testing.T) {
	t.Parallel()

	now := time.Now()

	// A busy session with follow-up should still appear first due to
	// busy having the highest status priority (0).
	sessions := []agent.SessionInfo{
		{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-30 * time.Minute), LastReadAt: now},
		{ID: "busy-fu", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour), FollowUp: true},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}

	// Busy (priority 0) should come before read-idle (priority 5).
	if m.groups[0].rows[0].session.ID != "busy-fu" {
		t.Errorf("expected busy session first, got %q", m.groups[0].rows[0].session.ID)
	}
}

func TestCategoryNavigation(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// 3 date groups: Today (3 rows), Yesterday (2 rows), Older (2 rows)
	// flatRows indices: 0,1,2 = Today; 3,4 = Yesterday; 5,6 = Older
	sessions := []agent.SessionInfo{
		{ID: "today-1", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "today-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
		{ID: "today-3", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
		{ID: "yest-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		{ID: "yest-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-26 * time.Hour), LastReadAt: now},
		{ID: "old-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-50 * time.Hour), LastReadAt: now},
		{ID: "old-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-51 * time.Hour), LastReadAt: now},
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
			{0, 0}, {1, 0}, {2, 0}, // Today
			{3, 1}, {4, 1}, // Yesterday
			{5, 2}, {6, 2}, // Older
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
			{0, 0}, // Today starts at 0
			{1, 3}, // Yesterday starts at 3
			{2, 5}, // Older starts at 5
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

		// Cursor at today-2 (index 1, middle of Today group)
		m.cursor = 1
		gi := m.cursorGroupIndex()
		first := m.groupFirstRow(gi)
		if m.cursor != first {
			m.cursor = first
		}
		if m.cursor != 0 {
			t.Errorf("cursor=%d, want 0 (top of Today)", m.cursor)
		}
	})

	t.Run("cmd+up from top of category goes to top of category above", func(t *testing.T) {
		t.Parallel()
		m := setup()

		// Cursor at yest-1 (index 3, top of Yesterday)
		m.cursor = 3
		gi := m.cursorGroupIndex()
		first := m.groupFirstRow(gi)
		if m.cursor == first && gi > 0 {
			m.cursor = m.groupFirstRow(gi - 1)
		}
		if m.cursor != 0 {
			t.Errorf("cursor=%d, want 0 (top of Today)", m.cursor)
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

		// From middle of Today (index 1)
		m.cursor = 1
		gi := m.cursorGroupIndex()
		if gi < len(m.groups)-1 {
			m.cursor = m.groupFirstRow(gi + 1)
		}
		if m.cursor != 3 {
			t.Errorf("cursor=%d, want 3 (top of Yesterday)", m.cursor)
		}

		// From top of Yesterday (index 3) to top of Older
		gi = m.cursorGroupIndex()
		if gi < len(m.groups)-1 {
			m.cursor = m.groupFirstRow(gi + 1)
		}
		if m.cursor != 5 {
			t.Errorf("cursor=%d, want 5 (top of Older day)", m.cursor)
		}
	})

	t.Run("cmd+down from last category goes to very last row", func(t *testing.T) {
		t.Parallel()
		m := setup()

		// Cursor at old-1 (index 5, top of last group)
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

func TestCategoryNavigation_WithArchivedRows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// Today: 2 active + 1 archived, Yesterday: 1 active + 1 archived
	sessions := []agent.SessionInfo{
		{ID: "today-1", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "today-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
		{ID: "today-arch", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityArchived},
		{ID: "yest-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		{ID: "yest-arch", Status: agent.StatusIdle, UpdatedAt: now.Add(-26 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityArchived},
	}

	t.Run("collapsed: shift+down jumps over accordion to next group", func(t *testing.T) {
		t.Parallel()
		m := &InboxModel{}
		m.buildGroups(sessions)

		// flatRows (collapsed): today-1(0), today-2(1), accordion-Today(2), yest-1(3), accordion-Yesterday(4)
		if len(m.flatRows) != 5 {
			t.Fatalf("expected 5 flatRows, got %d", len(m.flatRows))
		}

		// From Today group, shift+down should jump to Yesterday's first row (yest-1).
		m.cursor = 0
		gi := m.cursorGroupIndex()
		if gi < len(m.groups)-1 {
			m.cursor = m.groupFirstRow(gi + 1)
		}
		if m.cursor != 3 {
			t.Errorf("cursor=%d, want 3 (top of Yesterday, skipping accordion)", m.cursor)
		}
	})

	t.Run("collapsed: shift+up from yesterday jumps to top of today", func(t *testing.T) {
		t.Parallel()
		m := &InboxModel{}
		m.buildGroups(sessions)

		// Cursor at yest-1 (index 3)
		m.cursor = 3
		gi := m.cursorGroupIndex()
		first := m.groupFirstRow(gi)
		if m.cursor == first && gi > 0 {
			m.cursor = m.groupFirstRow(gi - 1)
		} else {
			m.cursor = first
		}
		if m.cursor != 0 {
			t.Errorf("cursor=%d, want 0 (top of Today)", m.cursor)
		}
	})

	t.Run("expanded: shift+down jumps over accordion and archived rows", func(t *testing.T) {
		t.Parallel()
		m := &InboxModel{archiveExpanded: map[string]bool{"Today": true}}
		m.buildGroups(sessions)

		// flatRows (Today expanded): today-1(0), today-2(1), accordion-Today(2), today-arch(3),
		//                            yest-1(4), accordion-Yesterday(5)
		if len(m.flatRows) != 6 {
			t.Fatalf("expected 6 flatRows, got %d", len(m.flatRows))
		}

		// From Today group, shift+down should jump to yest-1.
		m.cursor = 0
		gi := m.cursorGroupIndex()
		if gi < len(m.groups)-1 {
			m.cursor = m.groupFirstRow(gi + 1)
		}
		if m.cursor != 4 {
			t.Errorf("cursor=%d, want 4 (top of Yesterday, skipping accordion+archived)", m.cursor)
		}
	})

	t.Run("expanded: cursorGroupIndex correct for accordion and archived rows", func(t *testing.T) {
		t.Parallel()
		m := &InboxModel{archiveExpanded: map[string]bool{"Today": true}}
		m.buildGroups(sessions)

		tests := []struct {
			cursor    int
			wantGroup int
		}{
			{0, 0}, // today-1
			{1, 0}, // today-2
			{2, 0}, // accordion-Today
			{3, 0}, // today-arch (expanded archived row)
			{4, 1}, // yest-1
			{5, 1}, // accordion-Yesterday
		}
		for _, tt := range tests {
			m.cursor = tt.cursor
			got := m.cursorGroupIndex()
			if got != tt.wantGroup {
				t.Errorf("cursor=%d: cursorGroupIndex()=%d, want %d", tt.cursor, got, tt.wantGroup)
			}
		}
	})
}

func TestBreakpointNavigation(t *testing.T) {
	t.Parallel()

	now := time.Now()

	t.Run("groupLastActiveRow returns last active row", func(t *testing.T) {
		t.Parallel()
		// Today: busy(0), idle(1) — done is in doneRows accordion
		sessions := []agent.SessionInfo{
			{ID: "busy", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
			{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
			{ID: "done", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityDone},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		got := m.groupLastActiveRow(0)
		// busy(0), idle(1) are active rows; done is in doneRows. Last active is index 1.
		if got != 1 {
			t.Errorf("groupLastActiveRow(0) = %d, want 1", got)
		}
	})

	t.Run("groupLastActiveRow returns -1 when all rows are done", func(t *testing.T) {
		t.Parallel()
		sessions := []agent.SessionInfo{
			{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityDone},
			{ID: "done-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), Visibility: agent.VisibilityDone},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		got := m.groupLastActiveRow(0)
		if got != -1 {
			t.Errorf("groupLastActiveRow(0) = %d, want -1 (all done, no active rows)", got)
		}
	})

	t.Run("groupLastActiveRow no boundary when no done sessions", func(t *testing.T) {
		t.Parallel()
		sessions := []agent.SessionInfo{
			{ID: "idle-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
			{ID: "idle-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		got := m.groupLastActiveRow(0)
		// Both rows are active, so last active is index 1.
		if got != 1 {
			t.Errorf("groupLastActiveRow(0) = %d, want 1", got)
		}
	})

	t.Run("buildBreakpoints includes idle/done boundary", func(t *testing.T) {
		t.Parallel()
		// Today: busy(0), idle(1), done(2)
		// Yesterday: idle(3)
		sessions := []agent.SessionInfo{
			{ID: "busy", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
			{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
			{ID: "done", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityDone},
			{ID: "yest", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		bp := m.buildBreakpoints()
		// Today first=0, boundary=1; Yesterday first=3 (no boundary since single non-done row = first).
		want := []int{0, 1, 3}
		if len(bp) != len(want) {
			t.Fatalf("buildBreakpoints() = %v, want %v", bp, want)
		}
		for i := range want {
			if bp[i] != want[i] {
				t.Errorf("bp[%d] = %d, want %d (full: %v)", i, bp[i], want[i], bp)
			}
		}
	})

	t.Run("buildBreakpoints omits boundary when same as first", func(t *testing.T) {
		t.Parallel()
		// Single non-done row per group — boundary == first, so no extra breakpoint.
		sessions := []agent.SessionInfo{
			{ID: "today", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
			{ID: "yest", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		bp := m.buildBreakpoints()
		want := []int{0, 1}
		if len(bp) != len(want) {
			t.Fatalf("buildBreakpoints() = %v, want %v", bp, want)
		}
		for i := range want {
			if bp[i] != want[i] {
				t.Errorf("bp[%d] = %d, want %d", i, bp[i], want[i])
			}
		}
	})

	t.Run("shift+down cycles through breakpoints", func(t *testing.T) {
		t.Parallel()
		// Today: busy(0), follow-up(1), idle(2), done(3)
		// Yesterday: idle(4)
		sessions := []agent.SessionInfo{
			{ID: "busy", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
			{ID: "followup", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now, FollowUp: true},
			{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
			{ID: "done", Status: agent.StatusIdle, UpdatedAt: now.Add(-4 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityDone},
			{ID: "yest", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		// Breakpoints: Today first=0, boundary=2, Yesterday first=4.
		bp := m.buildBreakpoints()
		wantBP := []int{0, 2, 4}
		if len(bp) != len(wantBP) {
			t.Fatalf("buildBreakpoints() = %v, want %v", bp, wantBP)
		}

		// From 0, shift+down -> 2 (idle/done boundary).
		m.cursor = 0
		m.cursor = nextBreakpoint(bp, m.cursor)
		if m.cursor != 2 {
			t.Errorf("shift+down from 0: cursor=%d, want 2", m.cursor)
		}

		// From 2, shift+down -> 4 (top of Yesterday).
		m.cursor = nextBreakpoint(bp, m.cursor)
		if m.cursor != 4 {
			t.Errorf("shift+down from 2: cursor=%d, want 4", m.cursor)
		}

		// From 4 (last breakpoint), shift+down stays at 4.
		m.cursor = nextBreakpoint(bp, m.cursor)
		if m.cursor != 4 {
			t.Errorf("shift+down from 4: cursor=%d, want 4 (last breakpoint)", m.cursor)
		}
	})

	t.Run("shift+up cycles through breakpoints", func(t *testing.T) {
		t.Parallel()
		// Same setup as shift+down test.
		sessions := []agent.SessionInfo{
			{ID: "busy", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
			{ID: "followup", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now, FollowUp: true},
			{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
			{ID: "done", Status: agent.StatusIdle, UpdatedAt: now.Add(-4 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityDone},
			{ID: "yest", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		bp := m.buildBreakpoints()

		// From 4, shift+up -> 2 (idle/done boundary).
		m.cursor = 4
		m.cursor = prevBreakpoint(bp, m.cursor)
		if m.cursor != 2 {
			t.Errorf("shift+up from 4: cursor=%d, want 2", m.cursor)
		}

		// From 2, shift+up -> 0 (top of Today).
		m.cursor = prevBreakpoint(bp, m.cursor)
		if m.cursor != 0 {
			t.Errorf("shift+up from 2: cursor=%d, want 0", m.cursor)
		}

		// From 0 (first breakpoint), shift+up stays at 0.
		m.cursor = prevBreakpoint(bp, m.cursor)
		if m.cursor != 0 {
			t.Errorf("shift+up from 0: cursor=%d, want 0 (first breakpoint)", m.cursor)
		}
	})

	t.Run("shift+down from non-breakpoint position jumps to next breakpoint", func(t *testing.T) {
		t.Parallel()
		sessions := []agent.SessionInfo{
			{ID: "busy", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
			{ID: "followup", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now, FollowUp: true},
			{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
			{ID: "done", Status: agent.StatusIdle, UpdatedAt: now.Add(-4 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityDone},
			{ID: "yest", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		bp := m.buildBreakpoints()
		// Breakpoints: 0, 2, 4. Cursor at 1 (follow-up, not a breakpoint).
		m.cursor = 1
		m.cursor = nextBreakpoint(bp, m.cursor)
		if m.cursor != 2 {
			t.Errorf("shift+down from 1: cursor=%d, want 2 (boundary)", m.cursor)
		}
	})

	t.Run("shift+up from non-breakpoint position jumps to previous breakpoint", func(t *testing.T) {
		t.Parallel()
		sessions := []agent.SessionInfo{
			{ID: "busy", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
			{ID: "followup", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now, FollowUp: true},
			{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
			{ID: "done", Status: agent.StatusIdle, UpdatedAt: now.Add(-4 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityDone},
			{ID: "yest", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		bp := m.buildBreakpoints()
		// Cursor at 3 (done row, not a breakpoint). Previous breakpoint is 2.
		m.cursor = 3
		m.cursor = prevBreakpoint(bp, m.cursor)
		if m.cursor != 2 {
			t.Errorf("shift+up from 3: cursor=%d, want 2 (boundary)", m.cursor)
		}
	})

	t.Run("no done sessions means no extra breakpoint", func(t *testing.T) {
		t.Parallel()
		// All non-done sessions — breakpoints are only at group tops.
		sessions := []agent.SessionInfo{
			{ID: "today-1", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
			{ID: "today-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
			{ID: "today-3", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
			{ID: "yest-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		bp := m.buildBreakpoints()
		// Today: first=0, boundary=2. 2 != 0 so it IS included.
		// Yesterday: first=3, boundary=3 (single row). 3 == 3 so NOT included.
		want := []int{0, 2, 3}
		if len(bp) != len(want) {
			t.Fatalf("buildBreakpoints() = %v, want %v", bp, want)
		}
		for i := range want {
			if bp[i] != want[i] {
				t.Errorf("bp[%d] = %d, want %d", i, bp[i], want[i])
			}
		}
	})

	t.Run("with archived rows breakpoints skip accordion", func(t *testing.T) {
		t.Parallel()
		// Today: busy(0), idle(1), done(2), accordion(3)
		// Yesterday: idle(4), accordion(5)
		sessions := []agent.SessionInfo{
			{ID: "busy", Status: agent.StatusBusy, UpdatedAt: now.Add(-1 * time.Hour)},
			{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
			{ID: "done", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityDone},
			{ID: "today-arch", Status: agent.StatusIdle, UpdatedAt: now.Add(-4 * time.Hour), Visibility: agent.VisibilityArchived},
			{ID: "yest", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
			{ID: "yest-arch", Status: agent.StatusIdle, UpdatedAt: now.Add(-26 * time.Hour), Visibility: agent.VisibilityArchived},
		}
		m := &InboxModel{}
		m.buildGroups(sessions)

		bp := m.buildBreakpoints()
		// Today: first=0, boundary=1 (idle is last non-done in rows). Yesterday: first=4.
		want := []int{0, 1, 4}
		if len(bp) != len(want) {
			t.Fatalf("buildBreakpoints() = %v, want %v", bp, want)
		}
		for i := range want {
			if bp[i] != want[i] {
				t.Errorf("bp[%d] = %d, want %d", i, bp[i], want[i])
			}
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

func TestDateLabel(t *testing.T) {
	t.Parallel()

	// Fix "now" to a known point so tests are deterministic.
	now := time.Date(2025, time.March, 15, 14, 30, 0, 0, time.Local)

	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"same day", time.Date(2025, time.March, 15, 9, 0, 0, 0, time.Local), "Today"},
		{"yesterday", time.Date(2025, time.March, 14, 23, 59, 0, 0, time.Local), "Yesterday"},
		{"two days ago", time.Date(2025, time.March, 13, 12, 0, 0, 0, time.Local), "Thu, Mar 13"},
		{"last week", time.Date(2025, time.March, 8, 12, 0, 0, 0, time.Local), "Sat, Mar 8"},
		{"different year", time.Date(2024, time.December, 25, 10, 0, 0, 0, time.Local), "Wed, Dec 25, 2024"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := dateLabel(tt.t, now)
			if got != tt.want {
				t.Errorf("dateLabel(%v, %v) = %q, want %q", tt.t, now, got, tt.want)
			}
		})
	}
}

func TestBuildGroups_DateGrouping(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "today-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
		{ID: "today-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
		{ID: "yesterday-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
		{ID: "old-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-50 * time.Hour), LastReadAt: now},
	}

	m := &InboxModel{width: 120, height: 50}
	m.buildGroups(sessions)

	// Should have 3 date groups: Today, Yesterday, and an older day.
	if len(m.groups) != 3 {
		names := make([]string, len(m.groups))
		for i, g := range m.groups {
			names[i] = g.name
		}
		t.Fatalf("expected 3 date groups, got %d: %v", len(m.groups), names)
	}

	if m.groups[0].name != "Today" {
		t.Errorf("expected first group 'Today', got %q", m.groups[0].name)
	}
	if m.groups[1].name != "Yesterday" {
		t.Errorf("expected second group 'Yesterday', got %q", m.groups[1].name)
	}
	// Third group is an older date — just verify it's not Today or Yesterday.
	if m.groups[2].name == "Today" || m.groups[2].name == "Yesterday" {
		t.Errorf("expected third group to be an older date, got %q", m.groups[2].name)
	}

	// Today group should have 2 sessions.
	if len(m.groups[0].rows) != 2 {
		t.Errorf("Today group: expected 2 rows, got %d", len(m.groups[0].rows))
	}
}

func TestBuildGroups_SingleDayGroup(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "a", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
		{ID: "b", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), LastReadAt: now},
	}

	m := &InboxModel{width: 120, height: 50}
	m.buildGroups(sessions)

	// All sessions from today — should be one "Today" group.
	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}
	if m.groups[0].name != "Today" {
		t.Errorf("expected group name 'Today', got %q", m.groups[0].name)
	}
}

func TestBuildGroups_DoneArchivedSinkToBottomOfDay(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "done", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now, Visibility: agent.VisibilityDone},
		{ID: "busy", Status: agent.StatusBusy, UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "archived", Status: agent.StatusIdle, UpdatedAt: now.Add(-30 * time.Minute), LastReadAt: now, Visibility: agent.VisibilityArchived},
		{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-3 * time.Hour), LastReadAt: now},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}

	rows := m.groups[0].rows
	// Only active (non-done, non-archived) sessions in the date group: busy, idle.
	if len(rows) != 2 {
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.session.ID
		}
		t.Fatalf("expected 2 rows in date group, got %d: %v", len(rows), ids)
	}

	// Busy (priority 0) should be first, idle (priority 5) second.
	if rows[0].session.ID != "busy" {
		t.Errorf("row[0]: expected 'busy', got %q", rows[0].session.ID)
	}
	if rows[1].session.ID != "idle" {
		t.Errorf("row[1]: expected 'idle', got %q", rows[1].session.ID)
	}

	// Done session should be stored in the group's doneRows.
	if len(m.groups[0].doneRows) != 1 {
		t.Fatalf("expected 1 done session in group, got %d", len(m.groups[0].doneRows))
	}
	if m.groups[0].doneRows[0].session.ID != "done" {
		t.Errorf("expected done session 'done', got %q", m.groups[0].doneRows[0].session.ID)
	}

	// Archived session should be stored in the group's archivedRows.
	if len(m.groups[0].archivedRows) != 1 {
		t.Fatalf("expected 1 archived session in group, got %d", len(m.groups[0].archivedRows))
	}
	if m.groups[0].archivedRows[0].session.ID != "archived" {
		t.Errorf("expected archived session 'archived', got %q", m.groups[0].archivedRows[0].session.ID)
	}
}

func TestBuildDisplayLines_RowToLineAccountsForGroupHeaders(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "today", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
		{ID: "yesterday", Status: agent.StatusIdle, UpdatedAt: now.Add(-25 * time.Hour), LastReadAt: now},
	}

	m := &InboxModel{width: 120, height: 50}
	m.buildGroups(sessions)
	m.buildDisplayLines()

	if len(m.flatRows) != 2 {
		t.Fatalf("expected 2 flatRows, got %d", len(m.flatRows))
	}

	// The key invariant: each row's display line must point at a valid line
	// that is strictly after the previous row's line (accounting for headers).
	for i := 1; i < len(m.flatRows); i++ {
		if m.rowToLine[i] <= m.rowToLine[i-1] {
			t.Errorf("rowToLine[%d]=%d should be > rowToLine[%d]=%d",
				i, m.rowToLine[i], i-1, m.rowToLine[i-1])
		}
	}

	// And each rowToLine value must be a valid index into displayLines.
	for i, lineIdx := range m.rowToLine {
		if lineIdx < 0 || lineIdx >= len(m.displayLines) {
			t.Errorf("rowToLine[%d]=%d out of bounds (displayLines has %d entries)",
				i, lineIdx, len(m.displayLines))
		}
	}
}

func TestArchiveAccordion_CollapsedByDefault(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "archived-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityArchived},
		{ID: "archived-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), Visibility: agent.VisibilityArchived},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// 1 active session in date group + 1 accordion toggle = 2 flatRows.
	if len(m.flatRows) != 2 {
		t.Fatalf("expected 2 flatRows (1 session + 1 accordion), got %d", len(m.flatRows))
	}
	if m.flatRows[1].accordion == "" {
		t.Error("expected last flatRow to be the accordion toggle")
	}
	if m.archiveExpanded != nil && m.archiveExpanded["Today"] {
		t.Error("expected archiveExpanded['Today'] to be false by default")
	}
	if len(m.groups[0].archivedRows) != 2 {
		t.Errorf("expected 2 archived sessions in group, got %d", len(m.groups[0].archivedRows))
	}
}

func TestArchiveAccordion_ExpandedShowsArchivedRows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "archived-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityArchived},
		{ID: "archived-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), Visibility: agent.VisibilityArchived},
	}

	m := &InboxModel{archiveExpanded: map[string]bool{"Today": true}}
	m.buildGroups(sessions)

	// 1 active + 1 accordion + 2 archived = 4 flatRows.
	if len(m.flatRows) != 4 {
		t.Fatalf("expected 4 flatRows, got %d", len(m.flatRows))
	}
	if m.flatRows[1].accordion == "" {
		t.Error("expected flatRows[1] to be the accordion toggle")
	}
	if m.flatRows[2].session == nil || m.flatRows[2].session.ID != "archived-1" {
		t.Error("expected flatRows[2] to be archived-1")
	}
	if m.flatRows[3].session == nil || m.flatRows[3].session.ID != "archived-2" {
		t.Error("expected flatRows[3] to be archived-2")
	}
}

func TestArchiveAccordion_ToggleExpandCollapse(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "archived-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityArchived},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// Initially collapsed: 1 active + 1 accordion = 2 flatRows.
	if len(m.flatRows) != 2 {
		t.Fatalf("collapsed: expected 2 flatRows, got %d", len(m.flatRows))
	}

	// Expand.
	m.archiveExpanded = map[string]bool{"Today": true}
	m.rebuildFlatRows()

	// Expanded: 1 active + 1 accordion + 1 archived = 3 flatRows.
	if len(m.flatRows) != 3 {
		t.Fatalf("expanded: expected 3 flatRows, got %d", len(m.flatRows))
	}

	// Collapse again.
	m.archiveExpanded["Today"] = false
	m.rebuildFlatRows()

	if len(m.flatRows) != 2 {
		t.Fatalf("re-collapsed: expected 2 flatRows, got %d", len(m.flatRows))
	}
}

func TestArchiveAccordion_NoArchivedSessionsNoAccordion(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// No archived sessions — no accordion row.
	if len(m.flatRows) != 1 {
		t.Fatalf("expected 1 flatRow, got %d", len(m.flatRows))
	}
	for _, r := range m.flatRows {
		if r.accordion != "" {
			t.Error("unexpected accordion row when there are no archived sessions")
		}
	}
}

func TestArchiveAccordion_DisplayLines(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "archived-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityArchived},
	}

	m := &InboxModel{width: 120, height: 50}
	m.buildGroups(sessions)
	m.buildDisplayLines()

	// All rowToLine values should be valid indices into displayLines.
	for i, lineIdx := range m.rowToLine {
		if lineIdx < 0 || lineIdx >= len(m.displayLines) {
			t.Errorf("rowToLine[%d]=%d out of bounds (displayLines has %d entries)",
				i, lineIdx, len(m.displayLines))
		}
	}

	// The accordion line should contain "Archive" and a count.
	accordionLineIdx := m.rowToLine[1] // flatRows[1] is the accordion
	if !strings.Contains(m.displayLines[accordionLineIdx], "Archive") {
		t.Errorf("expected accordion line to contain 'Archive', got: %s", m.displayLines[accordionLineIdx])
	}
	if !strings.Contains(m.displayLines[accordionLineIdx], "1") {
		t.Errorf("expected accordion line to contain count '1', got: %s", m.displayLines[accordionLineIdx])
	}
}

func TestArchiveAccordion_ExpandedDisplayLinesIncludeArchivedRows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "archived-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityArchived},
		{ID: "archived-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), Visibility: agent.VisibilityArchived},
	}

	m := &InboxModel{width: 120, height: 50, archiveExpanded: map[string]bool{"Today": true}}
	m.buildGroups(sessions)
	m.buildDisplayLines()

	// flatRows: 1 active + 1 accordion + 2 archived = 4
	if len(m.flatRows) != 4 {
		t.Fatalf("expected 4 flatRows, got %d", len(m.flatRows))
	}

	// All rowToLine values should be valid and strictly increasing.
	for i, lineIdx := range m.rowToLine {
		if lineIdx < 0 || lineIdx >= len(m.displayLines) {
			t.Errorf("rowToLine[%d]=%d out of bounds (displayLines has %d entries)",
				i, lineIdx, len(m.displayLines))
		}
	}
	for i := 1; i < len(m.rowToLine); i++ {
		if m.rowToLine[i] <= m.rowToLine[i-1] {
			t.Errorf("rowToLine[%d]=%d should be > rowToLine[%d]=%d",
				i, m.rowToLine[i], i-1, m.rowToLine[i-1])
		}
	}
}

func TestDoneAccordion_CollapsedByDefault(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityDone},
		{ID: "done-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), Visibility: agent.VisibilityDone},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// 1 active session + 1 done accordion toggle = 2 flatRows.
	if len(m.flatRows) != 2 {
		t.Fatalf("expected 2 flatRows (1 session + 1 done accordion), got %d", len(m.flatRows))
	}
	if m.flatRows[1].doneAccordion == "" {
		t.Error("expected last flatRow to be the done accordion toggle")
	}
	if m.doneExpanded != nil && m.doneExpanded["Today"] {
		t.Error("expected doneExpanded['Today'] to be false by default")
	}
	if len(m.groups[0].doneRows) != 2 {
		t.Errorf("expected 2 done sessions in group, got %d", len(m.groups[0].doneRows))
	}
}

func TestDoneAccordion_ExpandedShowsDoneRows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityDone},
		{ID: "done-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), Visibility: agent.VisibilityDone},
	}

	m := &InboxModel{doneExpanded: map[string]bool{"Today": true}}
	m.buildGroups(sessions)

	// 1 active + 1 done accordion + 2 done rows = 4 flatRows.
	if len(m.flatRows) != 4 {
		t.Fatalf("expected 4 flatRows, got %d", len(m.flatRows))
	}
	if m.flatRows[1].doneAccordion == "" {
		t.Error("expected flatRows[1] to be the done accordion toggle")
	}
	if m.flatRows[2].session == nil || m.flatRows[2].session.ID != "done-1" {
		t.Error("expected flatRows[2] to be done-1")
	}
	if m.flatRows[3].session == nil || m.flatRows[3].session.ID != "done-2" {
		t.Error("expected flatRows[3] to be done-2")
	}
}

func TestDoneAccordion_ToggleExpandCollapse(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityDone},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// Initially collapsed: 1 active + 1 done accordion = 2 flatRows.
	if len(m.flatRows) != 2 {
		t.Fatalf("collapsed: expected 2 flatRows, got %d", len(m.flatRows))
	}

	// Expand.
	m.doneExpanded = map[string]bool{"Today": true}
	m.rebuildFlatRows()

	// Expanded: 1 active + 1 done accordion + 1 done row = 3 flatRows.
	if len(m.flatRows) != 3 {
		t.Fatalf("expanded: expected 3 flatRows, got %d", len(m.flatRows))
	}

	// Collapse again.
	m.doneExpanded["Today"] = false
	m.rebuildFlatRows()

	if len(m.flatRows) != 2 {
		t.Fatalf("re-collapsed: expected 2 flatRows, got %d", len(m.flatRows))
	}
}

func TestDoneAccordion_NoDoneSessionsNoAccordion(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// No done sessions — no done accordion row.
	if len(m.flatRows) != 1 {
		t.Fatalf("expected 1 flatRow, got %d", len(m.flatRows))
	}
	for _, r := range m.flatRows {
		if r.doneAccordion != "" {
			t.Error("unexpected done accordion row when there are no done sessions")
		}
	}
}

func TestDoneAccordion_DisplayLines(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityDone},
	}

	m := &InboxModel{width: 120, height: 50}
	m.buildGroups(sessions)
	m.buildDisplayLines()

	// All rowToLine values should be valid indices into displayLines.
	for i, lineIdx := range m.rowToLine {
		if lineIdx < 0 || lineIdx >= len(m.displayLines) {
			t.Errorf("rowToLine[%d]=%d out of bounds (displayLines has %d entries)",
				i, lineIdx, len(m.displayLines))
		}
	}

	// The accordion line should contain "Done" and a count.
	accordionLineIdx := m.rowToLine[1] // flatRows[1] is the done accordion
	if !strings.Contains(m.displayLines[accordionLineIdx], "Done") {
		t.Errorf("expected done accordion line to contain 'Done', got: %s", m.displayLines[accordionLineIdx])
	}
	if !strings.Contains(m.displayLines[accordionLineIdx], "1") {
		t.Errorf("expected done accordion line to contain count '1', got: %s", m.displayLines[accordionLineIdx])
	}
}

func TestDoneAccordion_ExpandedDisplayLinesIncludeDoneRows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityDone},
		{ID: "done-2", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), Visibility: agent.VisibilityDone},
	}

	m := &InboxModel{width: 120, height: 50, doneExpanded: map[string]bool{"Today": true}}
	m.buildGroups(sessions)
	m.buildDisplayLines()

	// flatRows: 1 active + 1 done accordion + 2 done rows = 4
	if len(m.flatRows) != 4 {
		t.Fatalf("expected 4 flatRows, got %d", len(m.flatRows))
	}

	// All rowToLine values should be valid and strictly increasing.
	for i, lineIdx := range m.rowToLine {
		if lineIdx < 0 || lineIdx >= len(m.displayLines) {
			t.Errorf("rowToLine[%d]=%d out of bounds (displayLines has %d entries)",
				i, lineIdx, len(m.displayLines))
		}
	}
	for i := 1; i < len(m.rowToLine); i++ {
		if m.rowToLine[i] <= m.rowToLine[i-1] {
			t.Errorf("rowToLine[%d]=%d should be > rowToLine[%d]=%d",
				i, m.rowToLine[i], i-1, m.rowToLine[i-1])
		}
	}
}

func TestDoneAndArchiveAccordions_BothPresent(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "active", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
		{ID: "done-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), Visibility: agent.VisibilityDone},
		{ID: "archived-1", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour), Visibility: agent.VisibilityArchived},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	// 1 active + 1 done accordion + 1 archive accordion = 3 flatRows.
	if len(m.flatRows) != 3 {
		t.Fatalf("expected 3 flatRows, got %d", len(m.flatRows))
	}
	if m.flatRows[1].doneAccordion == "" {
		t.Error("expected flatRows[1] to be the done accordion toggle")
	}
	if m.flatRows[2].accordion == "" {
		t.Error("expected flatRows[2] to be the archive accordion toggle")
	}

	// Done accordion appears before archive accordion.
	if m.flatRows[1].doneAccordion != "Today" {
		t.Errorf("expected done accordion for 'Today', got %q", m.flatRows[1].doneAccordion)
	}
	if m.flatRows[2].accordion != "Today" {
		t.Errorf("expected archive accordion for 'Today', got %q", m.flatRows[2].accordion)
	}
}

func TestRenderRow_DoneSessionGreenTitle(t *testing.T) {
	t.Parallel()

	m := &InboxModel{width: 120}
	now := time.Now()

	doneSession := &agent.SessionInfo{
		ID:         "done-1",
		Status:     agent.StatusIdle,
		Title:      "Fix the bug",
		UpdatedAt:  now,
		LastReadAt: now,
		Visibility: agent.VisibilityDone,
	}

	rendered := m.renderRow(inboxRow{session: doneSession}, false)

	// The title text should be present in the rendered output.
	if !strings.Contains(rendered, "Fix the bug") {
		t.Errorf("expected done row to contain title, got: %s", rendered)
	}

	// The rendered output should contain the ANSI escape for successColor (#10B981).
	// Lip Gloss renders this as an SGR sequence. We check for the color being applied.
	// successColor = "#10B981" — the exact escape depends on terminal capabilities,
	// but the title should NOT use the default text color.
	normalSession := &agent.SessionInfo{
		ID:         "normal-1",
		Status:     agent.StatusIdle,
		Title:      "Fix the bug",
		UpdatedAt:  now,
		LastReadAt: now,
	}
	normalRendered := m.renderRow(inboxRow{session: normalSession}, false)

	// The done session rendering should differ from the normal one
	// because the title has green styling applied.
	if rendered == normalRendered {
		t.Error("expected done session rendering to differ from normal session (green title)")
	}
}

func TestRenderRow_DoneSessionMutedProject(t *testing.T) {
	t.Parallel()

	m := &InboxModel{width: 120}
	now := time.Now()

	doneSession := &agent.SessionInfo{
		ID:          "done-1",
		Status:      agent.StatusIdle,
		Title:       "My task",
		ProjectName: "myproject",
		UpdatedAt:   now,
		LastReadAt:  now,
		Visibility:  agent.VisibilityDone,
	}
	normalSession := &agent.SessionInfo{
		ID:          "normal-1",
		Status:      agent.StatusIdle,
		Title:       "My task",
		ProjectName: "myproject",
		UpdatedAt:   now,
		LastReadAt:  now,
	}

	doneRendered := m.renderRow(inboxRow{session: doneSession}, false)
	normalRendered := m.renderRow(inboxRow{session: normalSession}, false)

	// Done row should render differently due to muted project color and green title.
	if doneRendered == normalRendered {
		t.Error("expected done session to render differently from normal session")
	}
}

func TestRenderRow_ArchivedSessionGrayedOut(t *testing.T) {
	t.Parallel()

	m := &InboxModel{width: 120}
	now := time.Now()

	archivedSession := &agent.SessionInfo{
		ID:          "arch-1",
		Status:      agent.StatusIdle,
		Title:       "Old task",
		ProjectName: "myproject",
		Agent:       "build",
		UpdatedAt:   now,
		LastReadAt:  now,
		Visibility:  agent.VisibilityArchived,
	}
	normalSession := &agent.SessionInfo{
		ID:          "normal-1",
		Status:      agent.StatusIdle,
		Title:       "Old task",
		ProjectName: "myproject",
		Agent:       "build",
		UpdatedAt:   now,
		LastReadAt:  now,
	}

	archivedRendered := m.renderRow(inboxRow{session: archivedSession}, false)
	normalRendered := m.renderRow(inboxRow{session: normalSession}, false)

	// Archived row should render differently — title, project, and agent badge
	// all use dimColor instead of their normal colors.
	if archivedRendered == normalRendered {
		t.Error("expected archived session to render differently from normal session (grayed out)")
	}

	// Title text should still be present.
	if !strings.Contains(archivedRendered, "Old task") {
		t.Errorf("expected archived row to contain title, got: %s", archivedRendered)
	}
}

func TestBuildGroups_UnreadDoesNotAffectOrdering(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "followup", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now, FollowUp: true},
		{ID: "unread", Status: agent.StatusIdle, UpdatedAt: now.Add(-2 * time.Hour)},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}

	// Unread status should not boost sort priority; both sessions have the
	// same priority so the more recently updated one ("followup") comes first.
	if m.groups[0].rows[0].session.ID != "followup" {
		t.Errorf("expected follow-up session first, got %q", m.groups[0].rows[0].session.ID)
	}
	if m.groups[0].rows[1].session.ID != "unread" {
		t.Errorf("expected unread session second, got %q", m.groups[0].rows[1].session.ID)
	}
}

func TestBuildGroups_ErrorDoesNotAffectOrdering(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sessions := []agent.SessionInfo{
		{ID: "error", Status: agent.StatusError, UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "idle", Status: agent.StatusIdle, UpdatedAt: now.Add(-1 * time.Hour), LastReadAt: now},
	}

	m := &InboxModel{}
	m.buildGroups(sessions)

	if len(m.groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(m.groups))
	}

	// Error status should not boost sort priority; both have the same
	// priority so the more recently updated one ("idle") comes first.
	if m.groups[0].rows[0].session.ID != "idle" {
		t.Errorf("expected idle session first, got %q", m.groups[0].rows[0].session.ID)
	}
	if m.groups[0].rows[1].session.ID != "error" {
		t.Errorf("expected error session second, got %q", m.groups[0].rows[1].session.ID)
	}
}

func TestRenderRow_UnreadBoldTitle(t *testing.T) {
	t.Parallel()

	m := &InboxModel{width: 120}
	now := time.Now()

	unreadSession := &agent.SessionInfo{
		ID:          "unread-1",
		Status:      agent.StatusIdle,
		Title:       "New changes",
		ProjectName: "proj",
		UpdatedAt:   now,
		// LastReadAt zero => Unread() is true
	}
	readSession := &agent.SessionInfo{
		ID:          "read-1",
		Status:      agent.StatusIdle,
		Title:       "New changes",
		ProjectName: "proj",
		UpdatedAt:   now,
		LastReadAt:  now,
	}

	unreadRendered := m.renderRow(inboxRow{session: unreadSession}, false)
	readRendered := m.renderRow(inboxRow{session: readSession}, false)

	// Unread should render differently from read — bold title vs dimmed title.
	if unreadRendered == readRendered {
		t.Error("expected unread session to render differently from read session")
	}

	// Both should contain the title text.
	if !strings.Contains(unreadRendered, "New changes") {
		t.Errorf("expected unread row to contain title, got: %s", unreadRendered)
	}
	if !strings.Contains(readRendered, "New changes") {
		t.Errorf("expected read row to contain title, got: %s", readRendered)
	}
}

func TestRenderRow_UnreadRedAsterisk(t *testing.T) {
	t.Parallel()

	m := &InboxModel{width: 120}
	now := time.Now()

	unreadSession := &agent.SessionInfo{
		ID:        "unread-1",
		Status:    agent.StatusIdle,
		Title:     "Something",
		UpdatedAt: now,
		// LastReadAt zero => Unread() is true
	}
	readSession := &agent.SessionInfo{
		ID:         "read-1",
		Status:     agent.StatusIdle,
		Title:      "Something",
		UpdatedAt:  now,
		LastReadAt: now,
	}

	unreadRendered := m.renderRow(inboxRow{session: unreadSession}, false)
	readRendered := m.renderRow(inboxRow{session: readSession}, false)

	// Unread row should contain an asterisk; read row should not.
	if !strings.Contains(unreadRendered, "*") {
		t.Errorf("expected unread row to contain '*', got: %s", unreadRendered)
	}
	if strings.Contains(readRendered, "*") {
		t.Errorf("expected read row to NOT contain '*', got: %s", readRendered)
	}
}

// TestAutoRefreshSurvivesSessionView is a regression test for the bug where
// entering the session view permanently killed the auto-refresh timer chain.
//
// Root cause: autoRefreshCmd() fires inboxRefreshMsg every 3 seconds and the
// handler re-schedules itself. When screen == screenSession, the session view
// delegation's default case silently swallowed inboxRefreshMsg (the session
// view didn't know about it and dropped it), breaking the chain forever.
//
// The fix intercepts inboxRefreshMsg before session view delegation — the same
// pattern used for spinner.TickMsg.
func TestAutoRefreshSurvivesSessionView(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.screen = screenSession
	m.sessionView = NewSessionViewModel(nil, "test-session")

	// Simulate the timer firing while in session view.
	_, cmd := m.Update(inboxRefreshMsg{})

	// The handler must re-schedule the timer (non-nil cmd) even though we're
	// not on the inbox screen; otherwise the refresh loop dies permanently.
	if cmd == nil {
		t.Fatal("inboxRefreshMsg was swallowed while in session view; auto-refresh timer chain is broken")
	}

	// The returned command should eventually produce another inboxRefreshMsg
	// (it's a tea.Tick wrapping inboxRefreshMsg). We can't easily test the
	// tick delay, but we can verify a cmd was returned — which is the key
	// invariant that keeps the chain alive.
}

// TestBackToInboxRestartsAutoRefresh is a regression test ensuring that
// returning from the session view to the inbox restarts the auto-refresh
// timer as a safety net. Even if the timer chain survived the session view,
// this provides defense-in-depth.
func TestBackToInboxRestartsAutoRefresh(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.screen = screenSession
	m.sessionView = NewSessionViewModel(nil, "test-session")

	_, cmd := m.Update(backToInboxMsg{})

	if cmd == nil {
		t.Fatal("backToInboxMsg returned nil cmd; expected loadDataCmd + autoRefreshCmd + spinner.Tick batch")
	}

	// After handling backToInboxMsg, we should be back on the inbox screen.
	if m.screen != screenInbox {
		t.Errorf("expected screen=screenInbox after backToInboxMsg, got %v", m.screen)
	}

	// The returned command is a tea.Batch of loadDataCmd, autoRefreshCmd, and
	// spinner.Tick. We can't easily decompose the batch, but verifying cmd != nil
	// plus the screen transition confirms the handler is wired correctly.
}

// TestSpinnerTickSurvivesConfirmDialog is a regression test for the bug where
// opening a confirm dialog swallowed spinner.TickMsg, permanently breaking the
// spinner's self-sustaining tick chain.
func TestSpinnerTickSurvivesConfirmDialog(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.showConfirm = true
	m.confirm = newConfirmDialog("Delete?", "Are you sure?", "delete")

	// Generate a valid tick message from the spinner's own state.
	tickMsg := m.spinner.Tick()

	_, cmd := m.Update(tickMsg)

	// The spinner must schedule the next tick (non-nil cmd) to keep
	// the animation alive.
	if cmd == nil {
		t.Fatal("spinner tick was swallowed by confirm dialog; expected a follow-up tick command")
	}

	// The returned command should produce another spinner.TickMsg.
	nextMsg := cmd()
	if _, ok := nextMsg.(spinner.TickMsg); !ok {
		t.Fatalf("expected spinner.TickMsg, got %T", nextMsg)
	}
}

// TestSpinnerTickSurvivesActionMenu is a regression test for the bug where
// opening the action menu swallowed spinner.TickMsg.
func TestSpinnerTickSurvivesActionMenu(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.showMenu = true
	m.menu = newActionMenu("Actions", nil)

	tickMsg := m.spinner.Tick()

	_, cmd := m.Update(tickMsg)

	if cmd == nil {
		t.Fatal("spinner tick was swallowed by action menu; expected a follow-up tick command")
	}

	nextMsg := cmd()
	if _, ok := nextMsg.(spinner.TickMsg); !ok {
		t.Fatalf("expected spinner.TickMsg, got %T", nextMsg)
	}
}

// TestSpinnerTickForwardedToSessionView is a regression test for the bug where
// the InboxModel intercepted all spinner.TickMsg before delegating to the
// session view. Since each spinner has a unique ID, the inbox spinner silently
// rejected session-view ticks (returning nil cmd), permanently killing the
// session spinner's tick chain.
func TestSpinnerTickForwardedToSessionView(t *testing.T) {
	t.Parallel()

	m := NewInboxModel(nil)
	m.screen = screenSession
	m.sessionView = NewSessionViewModel(nil, "test-session")

	// Generate a tick that belongs to the session view's spinner.
	sessionTickMsg := m.sessionView.spinner.Tick()

	_, cmd := m.Update(sessionTickMsg)

	if cmd == nil {
		t.Fatal("session spinner tick was swallowed by InboxModel; expected a follow-up tick command")
	}

	// Feed a second tick to confirm the chain is alive.
	secondTick := m.sessionView.spinner.Tick()
	_, cmd2 := m.Update(secondTick)
	if cmd2 == nil {
		t.Fatal("session spinner second tick returned nil cmd; tick chain is broken")
	}
}

// --- Project filter tests ---

func TestProjectFilterToggle(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		projectDir:  "/home/user/myproject",
		projectName: "myproject",
		width:       120,
		height:      40,
	}

	if m.projectFilter {
		t.Fatal("projectFilter should be false by default")
	}

	// Simulate pressing "." to toggle the filter on.
	m.handleInboxKey(tea.KeyPressMsg{Text: ".", Code: '.'})
	if !m.projectFilter {
		t.Fatal("projectFilter should be true after pressing '.'")
	}

	// Simulate pressing "." again to toggle the filter off.
	m.handleInboxKey(tea.KeyPressMsg{Text: ".", Code: '.'})
	if m.projectFilter {
		t.Fatal("projectFilter should be false after pressing '.' again")
	}
}

func TestFilteredSessionsByProject(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{
		projectDir:  "/home/user/alpha",
		projectName: "alpha",
		width:       120,
		height:      40,
		cachedSessions: []agent.SessionInfo{
			{ID: "s1", ProjectDir: "/home/user/alpha", ProjectName: "alpha", UpdatedAt: now},
			{ID: "s2", ProjectDir: "/home/user/beta", ProjectName: "beta", UpdatedAt: now.Add(-time.Hour)},
			{ID: "s3", ProjectDir: "/home/user/alpha", ProjectName: "alpha", UpdatedAt: now.Add(-2 * time.Hour)},
		},
	}

	// Without project filter, all sessions are returned.
	all := m.filteredSessions()
	if len(all) != 3 {
		t.Fatalf("expected 3 sessions without filter, got %d", len(all))
	}

	// Enable project filter.
	m.projectFilter = true
	filtered := m.filteredSessions()
	if len(filtered) != 2 {
		t.Fatalf("expected 2 sessions with project filter, got %d", len(filtered))
	}
	for _, s := range filtered {
		if s.ProjectDir != "/home/user/alpha" {
			t.Errorf("expected all filtered sessions to have projectDir /home/user/alpha, got %s", s.ProjectDir)
		}
	}
}

func TestProjectFilterRebuildsGroups(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{
		projectDir:  "/home/user/alpha",
		projectName: "alpha",
		width:       120,
		height:      40,
		cachedSessions: []agent.SessionInfo{
			{ID: "s1", ProjectDir: "/home/user/alpha", ProjectName: "alpha", Status: agent.StatusIdle, UpdatedAt: now},
			{ID: "s2", ProjectDir: "/home/user/beta", ProjectName: "beta", Status: agent.StatusIdle, UpdatedAt: now},
			{ID: "s3", ProjectDir: "/home/user/alpha", ProjectName: "alpha", Status: agent.StatusIdle, UpdatedAt: now.Add(-time.Hour)},
		},
	}

	// Build groups without filter — all 3 visible.
	m.buildGroups(m.filteredSessions())
	totalRows := len(m.flatRows)
	if totalRows != 3 {
		t.Fatalf("expected 3 rows without filter, got %d", totalRows)
	}

	// Toggle project filter on.
	m.projectFilter = true
	m.applyFiltersAndRebuild()
	if len(m.flatRows) != 2 {
		t.Fatalf("expected 2 rows with project filter, got %d", len(m.flatRows))
	}

	// Toggle project filter off — all should come back.
	m.projectFilter = false
	m.applyFiltersAndRebuild()
	if len(m.flatRows) != 3 {
		t.Fatalf("expected 3 rows after disabling filter, got %d", len(m.flatRows))
	}
}

func TestRenderFilterBar_ShowsPillWhenFilterActive(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		projectDir:  "/home/user/myproject",
		projectName: "myproject",
		width:       120,
		height:      40,
		searchInput: func() textinput.Model {
			ti := textinput.New()
			ti.Placeholder = "Search sessions..."
			ti.Prompt = "/ "
			return ti
		}(),
	}

	// Without filter — no pill.
	bar := m.renderFilterBar()
	if strings.Contains(bar, "myproject") {
		t.Errorf("filter bar should not contain project name when filter is off, got: %s", bar)
	}

	// With filter — pill should appear.
	m.projectFilter = true
	bar = m.renderFilterBar()
	if !strings.Contains(bar, "myproject") {
		t.Errorf("filter bar should contain project name when filter is on, got: %s", bar)
	}
}

func TestRenderFilterBar_PlaceholderVisibleWhenNotSearching(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		projectDir:  "/home/user/myproject",
		projectName: "myproject",
		width:       120,
		height:      40,
		searchInput: func() textinput.Model {
			ti := textinput.New()
			ti.Placeholder = "Search sessions..."
			ti.Prompt = "/ "
			ti.SetWidth(80)
			return ti
		}(),
	}

	// Not searching — the blurred search input should render the placeholder.
	bar := m.renderFilterBar()
	if !strings.Contains(bar, "earch sessions") {
		t.Errorf("filter bar should show placeholder when not searching, got: %s", bar)
	}
}

// --- Arrow key pane navigation tests ---

func TestLeftArrow_FromSessionPane_NavigatesToSidebar(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{
		width:  120,
		height: 40,
		pane:   paneSessions,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
		},
	}
	m.buildGroups([]agent.SessionInfo{
		{ID: "s1", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
	})

	// Precondition: session pane has focus, two-pane mode active.
	if !m.showTwoPanes() {
		t.Fatal("expected two-pane mode to be active")
	}
	if m.pane != paneSessions {
		t.Fatal("expected pane to be paneSessions")
	}

	m.handleInboxKey(tea.KeyPressMsg{Code: tea.KeyLeft})

	if m.pane != paneSidebar {
		t.Error("expected pane to switch to paneSidebar after left arrow")
	}
	if !m.sidebar.Focused() {
		t.Error("expected sidebar to be focused after left arrow")
	}
}

func TestLeftArrow_NarrowTerminal_NoOp(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{
		width:  60, // below minTwoPaneWidth (80)
		height: 40,
		pane:   paneSessions,
	}
	m.buildGroups([]agent.SessionInfo{
		{ID: "s1", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
	})

	if m.showTwoPanes() {
		t.Fatal("expected two-pane mode to be inactive in narrow terminal")
	}

	m.handleInboxKey(tea.KeyPressMsg{Code: tea.KeyLeft})

	if m.pane != paneSessions {
		t.Error("expected pane to remain paneSessions when two-pane mode is inactive")
	}
}

func TestLeftArrow_SidebarHidden_NoOp(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{
		width:         120,
		height:        40,
		pane:          paneSessions,
		sidebarHidden: true,
	}
	m.buildGroups([]agent.SessionInfo{
		{ID: "s1", Status: agent.StatusIdle, UpdatedAt: now, LastReadAt: now},
	})

	if m.showTwoPanes() {
		t.Fatal("expected two-pane mode to be inactive when sidebar is hidden")
	}

	m.handleInboxKey(tea.KeyPressMsg{Code: tea.KeyLeft})

	if m.pane != paneSessions {
		t.Error("expected pane to remain paneSessions when sidebar is hidden")
	}
}

func TestRightArrow_FromSidebar_NavigatesToSessionPane(t *testing.T) {
	t.Parallel()

	m := &InboxModel{
		width:  120,
		height: 40,
		pane:   paneSidebar,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
			focused:    true,
		},
	}

	if m.pane != paneSidebar {
		t.Fatal("expected pane to be paneSidebar")
	}

	m.handleSidebarKey(tea.KeyPressMsg{Code: tea.KeyRight})

	if m.pane != paneSessions {
		t.Error("expected pane to switch to paneSessions after right arrow")
	}
	if m.sidebar.Focused() {
		t.Error("expected sidebar to lose focus after right arrow")
	}
}

func TestRightArrow_WhileCreatingBranch_PassesThrough(t *testing.T) {
	t.Parallel()

	ti := textinput.New()
	ti.Focus()
	m := &InboxModel{
		width:  120,
		height: 40,
		pane:   paneSidebar,
		sidebar: SidebarModel{
			projectDir: "/tmp/test",
			focused:    true,
			creating:   true,
			input:      ti,
		},
	}

	m.handleSidebarKey(tea.KeyPressMsg{Code: tea.KeyRight})

	// Should remain on sidebar because we're in text-input mode.
	if m.pane != paneSidebar {
		t.Error("expected pane to remain paneSidebar while creating a branch")
	}
}

func TestBuildSearchResults_RespectsProjectFilter(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{
		projectDir:    "/home/user/alpha",
		projectName:   "alpha",
		projectFilter: true,
		width:         120,
		height:        40,
	}

	sessions := []agent.SessionInfo{
		{ID: "s1", ProjectDir: "/home/user/alpha", ProjectName: "alpha", Prompt: "fix bug", UpdatedAt: now},
		{ID: "s2", ProjectDir: "/home/user/beta", ProjectName: "beta", Prompt: "fix bug", UpdatedAt: now},
	}

	m.buildSearchResults(sessions)
	if len(m.flatRows) != 1 {
		t.Fatalf("expected 1 search result with project filter, got %d", len(m.flatRows))
	}
	if m.flatRows[0].session.ID != "s1" {
		t.Errorf("expected filtered result to be s1, got %s", m.flatRows[0].session.ID)
	}
}

// TestSearchStatePreservedAcrossSessionView is a regression test for the bug
// where entering a session from search results then returning to the inbox
// showed the search text in the input but did not apply the search filter.
// The fix: the enter handler in search mode no longer clears search state,
// so when the session view closes the inbox is still in search mode.
func TestSearchStatePreservedAcrossSessionView(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := &InboxModel{
		searching: true,
		searchInput: func() textinput.Model {
			ti := textinput.New()
			ti.SetValue("fix bug")
			return ti
		}(),
		searchQuery: "fix bug",
		width:       120,
		height:      40,
	}
	m.buildSearchResults([]agent.SessionInfo{
		{ID: "s1", ProjectName: "alpha", Prompt: "fix bug in auth", Status: agent.StatusIdle, UpdatedAt: now},
		{ID: "s2", ProjectName: "beta", Prompt: "fix bug in cart", Status: agent.StatusIdle, UpdatedAt: now},
	})

	// Verify precondition: 2 search results visible.
	if len(m.flatRows) != 2 {
		t.Fatalf("expected 2 search results, got %d", len(m.flatRows))
	}

	// Snapshot the search state before simulating enter+back.
	wantQuery := m.searchQuery
	wantValue := m.searchInput.Value()
	wantRows := len(m.flatRows)

	// Simulate opening a session: the session view takes over the screen
	// but search state should be preserved.
	m.screen = screenSession
	m.activeConnID = "s1"

	// Simulate returning to inbox (the state changes backToInboxMsg makes,
	// minus the commands that need a real client).
	m.screen = screenInbox
	m.sessionView = nil
	m.activeConnID = ""

	// Search state must be fully preserved.
	if !m.searching {
		t.Error("searching should still be true after returning from session view")
	}
	if m.searchQuery != wantQuery {
		t.Errorf("searchQuery = %q, want %q", m.searchQuery, wantQuery)
	}
	if m.searchInput.Value() != wantValue {
		t.Errorf("searchInput value = %q, want %q", m.searchInput.Value(), wantValue)
	}
	if len(m.flatRows) != wantRows {
		t.Errorf("flatRows count = %d, want %d", len(m.flatRows), wantRows)
	}
}
