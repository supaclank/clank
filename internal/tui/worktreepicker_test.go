package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/git"
)

func sampleWorktrees() []git.Worktree {
	return []git.Worktree{
		{Path: "/repo/main", Branch: "main"},
		{Path: "/repo/.wt/feat", Branch: "feat"},
		{Path: "/repo/.wt/fix", Branch: "fix"},
	}
}

func TestWorktreePicker_CursorStartsOnCurrent(t *testing.T) {
	t.Parallel()
	wp := newWorktreePicker(sampleWorktrees(), "/repo/.wt/feat", 10, nil)
	if wp.cursor != 1 {
		t.Fatalf("expected cursor on the current worktree (index 1), got %d", wp.cursor)
	}
}

func TestWorktreePicker_SelectEmitsResult(t *testing.T) {
	t.Parallel()
	wp := newWorktreePicker(sampleWorktrees(), "/repo/main", 10, nil)
	wp, _ = wp.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // → feat

	_, cmd := wp.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a result command")
	}
	res, ok := cmd().(worktreePickerResultMsg)
	if !ok {
		t.Fatalf("expected worktreePickerResultMsg, got %T", cmd())
	}
	if res.dir != "/repo/.wt/feat" {
		t.Fatalf("expected the highlighted worktree dir, got %q", res.dir)
	}
}

func TestWorktreePicker_FilterNarrows(t *testing.T) {
	t.Parallel()
	wts := []git.Worktree{
		{Path: "/repo/main", Branch: "main"},
		{Path: "/repo/.wt/feat-x", Branch: "feat-x"},
		{Path: "/repo/.wt/feat-y", Branch: "feat-y"},
	}
	wp := newWorktreePicker(wts, "/repo/main", 10, nil)
	wp.search.SetValue("feat")
	wp.applyFilter()

	got := map[string]bool{}
	for _, w := range wp.filtered {
		got[w.Branch] = true
	}
	if got["main"] || !got["feat-x"] || !got["feat-y"] {
		t.Fatalf("filter 'feat' should keep feat-x/feat-y only, got %v", got)
	}
}

// The selected row is rendered plain so the cursor's background highlight fills
// the whole line (inner color spans would reset the background mid-row).
func TestWorktreePicker_SelectedRowIsPlain(t *testing.T) {
	t.Parallel()
	wts := []git.Worktree{{Path: "/repo/main", Branch: "main"}}
	wp := newWorktreePicker(wts, "/repo/main", 10, nil)

	if label := wp.renderItem(wts[0], 56, true); strings.ContainsRune(label, '\x1b') {
		t.Fatalf("selected row must be plain (no ANSI) so the highlight fills; got %q", label)
	}
	if label := wp.renderItem(wts[0], 56, false); !strings.ContainsRune(label, '\x1b') {
		t.Fatal("non-selected current worktree should be styled (green dot)")
	}
}

// Regression: renderItem must not panic when the worktree path contains wide
// characters (emoji/CJK) where visual width > rune count.
func TestWorktreePicker_RenderItemNoPanicOnWideCharPath(t *testing.T) {
	t.Parallel()
	// 8 emoji path = visual width 16; with avail=12, old slice index went negative.
	wt := git.Worktree{Path: "🌟🌟🌟🌟🌟🌟🌟🌟", Branch: "main"}
	wp := newWorktreePicker([]git.Worktree{wt}, wt.Path, 10, nil)
	_ = wp.renderItem(wt, 20, false) // must not panic
}

func TestWorktreePicker_EscCancels(t *testing.T) {
	t.Parallel()
	wp := newWorktreePicker(sampleWorktrees(), "/repo/main", 10, nil)
	_, cmd := wp.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if _, ok := cmd().(worktreePickerCancelMsg); !ok {
		t.Fatalf("expected worktreePickerCancelMsg, got %T", cmd())
	}
}

// listWorktrees returns the repo's worktrees and drops the bare entry.
func TestListWorktrees_FiltersBareAndReadsRepo(t *testing.T) {
	t.Parallel()
	dir := initGitRepoForCompose(t) // a real git repo

	wts, err := listWorktrees(dir)
	if err != nil {
		t.Fatalf("listWorktrees: %v", err)
	}
	if len(wts) == 0 {
		t.Fatal("expected at least the main worktree")
	}
	for _, w := range wts {
		if w.Bare {
			t.Fatal("bare entry should be filtered out")
		}
	}
}

// --- compose integration ---

func TestCompose_WorktreeRowEnterOpensPicker(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	m = focusRow(t, m, focusWorktree)

	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.showWorktreePicker {
		t.Fatal("expected enter on the Worktree row to open the worktree picker")
	}
}

func TestCompose_WorktreePickerResultUpdatesProject(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	m.showWorktreePicker = true
	newDir := t.TempDir()

	model, _ := m.Update(worktreePickerResultMsg{dir: newDir})
	m = model.(*SessionViewModel)

	if m.showWorktreePicker {
		t.Fatal("expected the worktree picker to close after a result")
	}
	if m.projectDir != newDir {
		t.Fatalf("expected projectDir %q, got %q", newDir, m.projectDir)
	}
	if m.composeFocus != focusWorktree {
		t.Fatal("expected focus to stay on the Worktree row after picking")
	}
}
