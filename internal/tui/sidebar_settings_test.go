package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// TestSidebar_SettingsCursorIndex_NoBranches verifies the settings row sits
// immediately after the "All branches" entry when no branches exist.
func TestSidebar_SettingsCursorIndex_NoBranches(t *testing.T) {
	t.Parallel()

	m := SidebarModel{}
	// 0 = All, 1 = Settings footer.
	if got := m.settingsCursorIndex(); got != 1 {
		t.Errorf("settingsCursorIndex with 0 branches: got %d, want 1", got)
	}
}

func TestSidebar_SettingsCursorIndex_WithBranches(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		branches: []host.BranchInfo{{Name: "a"}, {Name: "b"}, {Name: "c"}},
	}
	// 0 = All, 1..3 = branches, 4 = Settings.
	if got := m.settingsCursorIndex(); got != 4 {
		t.Errorf("settingsCursorIndex with 3 branches: got %d, want 4", got)
	}
}

func TestSidebar_CursorOnSettings_TrackingMoves(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		branches: []host.BranchInfo{{Name: "main"}},
		focused:  true,
	}
	// Cursor defaults to 0 = "All".
	if m.CursorOnSettings() {
		t.Fatal("cursor should not be on settings at index 0")
	}

	// Down x 2 -> 0=All, 1=main, 2=Settings.
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if !m.CursorOnSettings() {
		t.Errorf("expected CursorOnSettings after 2x down; cursor=%d", m.cursor)
	}

	// Can't go past settings.
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.cursor != 2 {
		t.Errorf("cursor moved past settings: got %d, want 2", m.cursor)
	}
}

// TestSidebar_NKeyIgnoredOnSettings verifies that pressing "n" while parked
// on the settings row does NOT open the new-branch prompt. This prevents a
// confusing UX where "n" creates a branch from a non-branch row.
func TestSidebar_NKeyIgnoredOnSettings(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		branches: []host.BranchInfo{{Name: "main"}},
		focused:  true,
	}
	// Move cursor to settings row.
	m.cursor = m.settingsCursorIndex()

	m.handleKey(tea.KeyPressMsg{Text: "n", Code: 'n'})

	if m.creating {
		t.Error("expected creating=false when 'n' pressed on settings row")
	}
}

// TestSidebar_NKeyOnBranchStartsCreate sanity-checks that 'n' still works on
// a branch row — regression guard for the gate above.
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
// behaves like "no branch selected" so callers of SelectedBranch don't
// accidentally treat it as a branch name.
func TestSidebar_SelectedBranch_EmptyOnSettings(t *testing.T) {
	t.Parallel()

	m := SidebarModel{
		branches: []host.BranchInfo{{Name: "main"}, {Name: "dev"}},
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
