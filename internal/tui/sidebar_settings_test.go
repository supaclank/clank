package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/acksell/clank/internal/agent"
)

// TestSidebar_SettingsCursorIndex_NoEntries verifies the settings row sits
// after the import row when no entries exist: [0=All, 1=Import, 2=Settings].
func TestSidebar_SettingsCursorIndex_NoBranches(t *testing.T) {
	t.Parallel()

	m := SidebarModel{}
	// 0 = All, 1 = Import, 2 = Settings footer.
	if got := m.settingsCursorIndex(); got != 2 {
		t.Errorf("settingsCursorIndex with 0 entries: got %d, want 2", got)
	}
}

func TestSidebar_SettingsCursorIndex_WithBranches(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		entries: makeEntries(3),
	}
	// 0 = All, 1..3 = entries, 4 = Import, 5 = Settings.
	if got := m.settingsCursorIndex(); got != 5 {
		t.Errorf("settingsCursorIndex with 3 entries: got %d, want 5", got)
	}
}

func TestSidebar_CursorOnSettings_TrackingMoves(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		entries: makeEntries(1),
		focused: true,
	}
	// Cursor defaults to 0 = "All".
	if m.CursorOnSettings() {
		t.Fatal("cursor should not be on settings at index 0")
	}

	// Down x 3 -> 0=All, 1=entry, 2=Import, 3=Settings.
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if !m.CursorOnSettings() {
		t.Errorf("expected CursorOnSettings after 3x down; cursor=%d", m.cursor)
	}

	// Down from settings wraps back to the top (All).
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.cursor != 0 {
		t.Errorf("cursor did not wrap from settings to All: got %d, want 0", m.cursor)
	}
}

// TestSidebar_NKeyIgnoredOnSettings verifies that pressing "n" while parked
// on the settings row does NOT open the new-branch prompt.
func TestSidebar_NKeyIgnoredOnSettings(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		entries: makeEntries(1),
		focused: true,
	}
	// Move cursor to settings row.
	m.cursor = m.settingsCursorIndex()

	m.handleKey(tea.KeyPressMsg{Text: "n", Code: 'n'})

	if m.creating {
		t.Error("expected creating=false when 'n' pressed on settings row")
	}
}

// TestSidebar_NKeyOnBranchStartsCreate sanity-checks that 'n' still works on
// a worktree row — regression guard for the gate above.
func TestSidebar_NKeyOnBranchStartsCreate(t *testing.T) {
	t.Parallel()

	m := NewSidebarModel(nil, "local", agent.GitRef{LocalPath: "/tmp"}, "/tmp")
	m.focused = true
	m.cursor = 0 // "All"

	m.handleKey(tea.KeyPressMsg{Text: "n", Code: 'n'})

	if !m.creating {
		t.Error("expected 'n' to enter branch-creation mode from a non-settings row")
	}
}

// TestSidebar_FooterRendered verifies the "⚙ Settings" row shows up in the
// rendered view (so it's always discoverable).
func TestSidebar_FooterRendered(t *testing.T) {
	t.Parallel()

	m := SidebarModel{width: 30, height: 20}
	out := m.View()
	if !strings.Contains(out, "Settings") {
		t.Errorf("sidebar view missing 'Settings' footer:\n%s", out)
	}
}

// TestSidebar_SelectedBranch_EmptyOnSettings verifies the settings row
// behaves like "no worktree selected" so callers don't accidentally treat it
// as a worktree path.
func TestSidebar_SelectedBranch_EmptyOnSettings(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		entries: makeEntries(2),
	}
	m.cursor = m.settingsCursorIndex()

	if got := m.SelectedBranch(); got != "" {
		t.Errorf("SelectedBranch on settings row: got %q, want empty string", got)
	}
	if got := m.SelectedWorktreeDir(); got != "" {
		t.Errorf("SelectedWorktreeDir on settings row: got %q, want empty string", got)
	}
	if got := m.SelectedBranchInfo(); got != nil {
		t.Errorf("SelectedBranchInfo on settings row: got %+v, want nil", got)
	}
}
