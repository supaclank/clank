package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// mkRepo creates dir with a .git entry so dirIsGitRepo treats it as a repo
// root — cheaper than a real `git init` and all the picker inspects.
func mkRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mkSubdir(t *testing.T, parent, name string) string {
	t.Helper()
	d := filepath.Join(parent, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFolderPicker_SelectGitRepoEmitsResult(t *testing.T) {
	t.Parallel()
	repo := mkRepo(t, filepath.Join(t.TempDir(), "myrepo"))

	fp := newFolderPicker(repo) // current is a git repo; cursor on "Use this folder"
	_, cmd := fp.activate()
	if cmd == nil {
		t.Fatal("expected a result command when selecting a git repo")
	}
	res, ok := cmd().(folderPickerResultMsg)
	if !ok {
		t.Fatalf("expected folderPickerResultMsg, got %T", cmd())
	}
	if res.dir != repo {
		t.Fatalf("expected %q, got %q", repo, res.dir)
	}
}

func TestFolderPicker_NonRepoNotSelectable(t *testing.T) {
	t.Parallel()
	fp := newFolderPicker(t.TempDir()) // a plain (non-git) directory

	fp, cmd := fp.activate()
	if cmd != nil {
		t.Fatal("a non-git folder must not be selectable")
	}
	if fp.hint == "" {
		t.Fatal("expected a hint explaining the folder isn't a git repo")
	}
}

// Enter on a repo subdir selects it directly (this picker only ever selects
// repo roots), while → browses into it.
func TestFolderPicker_EnterSelectsRepoSubdir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := mkRepo(t, mkSubdir(t, root, "child"))

	cursorTo := func(fp folderPickerModel) folderPickerModel {
		for i, it := range fp.items {
			if it.kind == folderItemSubdir && it.name == "child" {
				fp.cursor = i
			}
		}
		return fp
	}

	// Enter selects the repo.
	fp := cursorTo(newFolderPicker(root))
	_, cmd := fp.activate()
	if cmd == nil {
		t.Fatal("expected Enter on a repo subdir to select it")
	}
	if r, ok := cmd().(folderPickerResultMsg); !ok || r.dir != child {
		t.Fatalf("expected result %q, got %#v", child, cmd())
	}

	// → browses into the repo instead of selecting.
	fp = cursorTo(newFolderPicker(root))
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if fp.current != child {
		t.Fatalf("expected → to browse into %q, got %q", child, fp.current)
	}
}

func TestFolderPicker_FilterNarrowsSubdirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkSubdir(t, root, "alpha")
	mkSubdir(t, root, "alfred")
	mkSubdir(t, root, "beta")

	fp := newFolderPicker(root)
	fp.applyQuery("al")

	names := map[string]bool{}
	for _, it := range fp.items {
		if it.kind == folderItemSubdir {
			names[it.name] = true
		}
	}
	if !names["alpha"] || !names["alfred"] || names["beta"] {
		t.Fatalf("filter 'al' should match alpha+alfred only, got %v", names)
	}
}

func TestFolderPicker_ParentNavigation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := mkSubdir(t, root, "child")

	fp := newFolderPicker(child)
	// Items: [Use this folder, ..]. Select ".." (index 1) to go up.
	fp.cursor = 1
	if fp.cursorItem().kind != folderItemParent {
		t.Fatalf("expected '..' at cursor 1, got kind %d", fp.cursorItem().kind)
	}
	fp, _ = fp.activate()
	if fp.current != root {
		t.Fatalf("expected to navigate up to %q, got %q", root, fp.current)
	}
}

// --- compose integration ---

// Cancelling the folder picker closes only the picker — compose stays open
// (so the picker's Esc doesn't get conflated with compose's exit-on-Esc).
func TestCompose_FolderPickerCancelStaysInCompose(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	m.showFolderPicker = true

	model, _ := m.Update(folderPickerCancelMsg{})
	m = model.(*SessionViewModel)
	if m.showFolderPicker {
		t.Fatal("cancel should close the folder picker")
	}
	if !m.composing {
		t.Fatal("cancelling the picker must not exit compose")
	}
}

// The folder picker opens browsing the parent (siblings listed) with the
// current project highlighted, since switching project is the common case.
func TestCompose_FolderPickerOpensAtSiblings(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	proj := mkSubdir(t, root, "myproj")
	mkSubdir(t, root, "sibling")

	m := newComposeForFocusTest(t)
	m.projectDir = proj
	m = focusRow(t, m, focusFolder)
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if !m.showFolderPicker {
		t.Fatal("expected the folder picker to open")
	}
	if m.folderPicker.current != root {
		t.Fatalf("expected the picker to browse the parent %q, got %q", root, m.folderPicker.current)
	}
	if it := m.folderPicker.cursorItem(); it.kind != folderItemSubdir || it.name != "myproj" {
		t.Fatalf("expected the cursor on the current project 'myproj', got %+v", it)
	}
}

func TestCompose_ProjectRowEnterOpensFolderPicker(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	m = focusRow(t, m, focusFolder)

	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.showFolderPicker {
		t.Fatal("expected enter on the Project row to open the folder picker")
	}
}

func TestCompose_FolderPickerResultUpdatesProject(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	m.showFolderPicker = true
	newDir := mkRepo(t, filepath.Join(t.TempDir(), "picked"))

	model, _ := m.Update(folderPickerResultMsg{dir: newDir})
	m = model.(*SessionViewModel)

	if m.showFolderPicker {
		t.Fatal("expected the folder picker to close after a result")
	}
	if m.projectDir != newDir {
		t.Fatalf("expected projectDir %q, got %q", newDir, m.projectDir)
	}
	if m.gitRef.LocalPath != newDir {
		t.Fatalf("expected gitRef.LocalPath %q, got %q", newDir, m.gitRef.LocalPath)
	}
	if m.composeFocus != focusFolder {
		t.Fatal("expected focus to stay on the Project row after picking a folder")
	}
}

func TestFolderPicker_LeftRightNavigatesDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := mkSubdir(t, root, "child")

	fp := newFolderPicker(root)
	// Move the cursor onto "child", then → descends into it.
	for i, it := range fp.items {
		if it.kind == folderItemSubdir && it.name == "child" {
			fp.cursor = i
		}
	}
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if fp.current != child {
		t.Fatalf("→ should descend into %q, got %q", child, fp.current)
	}
	// ← goes back up to the parent.
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if fp.current != root {
		t.Fatalf("← should go up to %q, got %q", root, fp.current)
	}
}

// The modal keeps a constant height as results change, so it doesn't jump.
func TestFolderPicker_FixedHeight(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, n := range []string{"alpha", "beta", "gamma", "delta"} {
		mkSubdir(t, root, n)
	}
	fp := newFolderPicker(root)

	full := strings.Count(fp.View(), "\n")
	fp.setFilter("alpha") // narrows to one result
	filtered := strings.Count(fp.View(), "\n")
	if full != filtered {
		t.Fatalf("modal height changed with results (%d vs %d) — it would jump", full, filtered)
	}
}

// Tab and "/" descend one level into the best match (like →).
func TestFolderPicker_TabAndSlashDescend(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkSubdir(t, root, "clank")
	mkSubdir(t, root, "clank-mobile")
	mkSubdir(t, root, "docs")

	tab := newFolderPicker(root)
	tab.setFilter("cl")
	tab, _ = tab.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if tab.current != filepath.Join(root, "clank") {
		t.Fatalf("expected Tab to descend into 'clank', got %q", tab.current)
	}

	slash := newFolderPicker(root)
	slash.setFilter("do")
	slash, _ = slash.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if slash.current != filepath.Join(root, "docs") {
		t.Fatalf("expected '/' to descend into 'docs', got %q", slash.current)
	}
}

// The cursor sits just after the separator (we're picking the folder *under*
// the current level), so it reads "current/█child", never "current█/child".
func TestFolderPicker_CursorFollowsSeparator(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkSubdir(t, root, "child")

	fp := newFolderPicker(root) // resting; previews the first child
	bc := ansi.Strip(fp.renderBreadcrumb(120))
	if !strings.Contains(bc, "/█") {
		t.Fatalf("expected the cursor after the separator ('/█'), got %q", bc)
	}
	if strings.Contains(bc, "█/") {
		t.Fatalf("did not expect the cursor before the separator ('█/'), got %q", bc)
	}
}

// While typing a prefix, the breadcrumb shows the rest of the best match as a
// ghost so the full name is visible (and Tab-completable).
func TestFolderPicker_GhostCompletion(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkSubdir(t, root, "clank-mobile")

	fp := newFolderPicker(root)
	fp.setFilter("clank-mob")
	// The cursor block sits between the typed text and the ghost; drop it so
	// the completed name reads contiguously.
	bc := strings.ReplaceAll(ansi.Strip(fp.renderBreadcrumb(120)), "█", "")
	if !strings.Contains(bc, "clank-mobile") {
		t.Fatalf("expected ghost to reveal 'clank-mobile', got %q", bc)
	}
}

// Backspacing up records the folder we left in the trail (same as ←), so the
// parent previews the folder we came from — not the alphabetically-first child.
func TestFolderPicker_BackspaceUpRecordsTrail(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkSubdir(t, root, "aaa-first") // sorts before "target"
	target := mkSubdir(t, root, "target")

	fp := newFolderPicker(target) // start inside target, filter empty
	// Backspace across the boundary, then delete the seeded text to empty.
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if fp.current != root {
		t.Fatalf("expected backspace to step up to %q, got %q", root, fp.current)
	}
	for fp.filter != "" {
		fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}

	if next, ok := fp.shownNextLevel(); !ok || next.name != "target" {
		t.Fatalf("expected the trail head 'target' previewed after backspacing up, got %q (ok=%v)", next.name, ok)
	}
}

// On a fresh directory (no memorized trail) the picker still previews a next
// level — the first subdirectory — so Tab keeps suggesting a way deeper.
func TestFolderPicker_PreviewsNextLevelWithoutTrail(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	alpha := mkSubdir(t, root, "alpha")
	mkSubdir(t, root, "beta")
	mkSubdir(t, alpha, "inner")

	fp := newFolderPicker(root) // fresh: no trail, cursor on "Use this folder"
	if next, ok := fp.shownNextLevel(); !ok || next.name != "alpha" {
		t.Fatalf("expected first child 'alpha' previewed with no trail, got %q (ok=%v)", next.name, ok)
	}

	// Tab descends into alpha; alpha is also fresh, but still previews 'inner'.
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if fp.current != alpha {
		t.Fatalf("expected Tab to descend into 'alpha', got %q", fp.current)
	}
	if next, ok := fp.shownNextLevel(); !ok || next.name != "inner" {
		t.Fatalf("expected 'inner' previewed after descending, got %q (ok=%v)", next.name, ok)
	}
}

// A segment matching no folder has no previewed next level (the cue that
// turns it red).
func TestFolderPicker_NoMatchHasNoNextLevel(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkSubdir(t, root, "foo")

	fp := newFolderPicker(root)
	fp.setFilter("zzz")
	if _, ok := fp.shownNextLevel(); ok {
		t.Fatal("expected no previewed next level for a non-matching filter")
	}
}

// Tab follows the breadcrumb, not the alphabetically-first child: at rest with
// a memorized trail, Tab descends into the trail head (what's shown), even
// when other subdirs sort earlier.
func TestFolderPicker_TabFollowsTrailNotFirstChild(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkSubdir(t, root, "aaa-first") // sorts before "target"
	target := mkSubdir(t, root, "target")

	fp := newFolderPicker(root)
	// Record a trail into "target" by descending and coming back.
	for i, it := range fp.items {
		if it.kind == folderItemSubdir && it.name == "target" {
			fp.cursor = i
		}
	}
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // into target
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyLeft})  // back to root (trail = target)
	fp.cursor = 0                                          // cursor on "Use this folder", as in the report

	next, ok := fp.shownNextLevel()
	if !ok || next.name != "target" {
		t.Fatalf("expected the previewed next level to be the trail head 'target', got %q (ok=%v)", next.name, ok)
	}
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if fp.current != target {
		t.Fatalf("expected Tab to descend into the trail head %q, got %q", target, fp.current)
	}
}

// Backspace with an empty typed segment crosses the "/" boundary: it steps up
// and seeds the parent's segment (minus a char) so editing flows continuously.
func TestFolderPicker_BackspaceCrossesBoundary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := mkSubdir(t, root, "branch")

	fp := newFolderPicker(child) // filter is empty at a directory
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})

	if fp.current != root {
		t.Fatalf("expected backspace to step up to %q, got %q", root, fp.current)
	}
	if fp.filter != "branc" {
		t.Fatalf("expected the parent's segment seeded minus a char ('branc'), got %q", fp.filter)
	}
}

// Typing a subdirectory's name makes "Use this folder" target THAT folder
// (the typed segment), not the committed parent — so a typed repo name is
// immediately selectable.
func TestFolderPicker_TypedSubdirNameIsSelectable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := mkRepo(t, mkSubdir(t, root, "myrepo")) // root/myrepo is a git repo

	fp := newFolderPicker(root) // root itself is not a repo
	if dirIsGitRepo(fp.targetDir()) {
		t.Fatal("setup: an empty filter should target the (non-repo) root")
	}

	fp.applyQuery("myrepo")
	if fp.targetDir() != repo {
		t.Fatalf("expected typing the name to target %q, got %q", repo, fp.targetDir())
	}

	fp.cursor = 0 // "Use this folder"
	_, cmd := fp.activate()
	if cmd == nil {
		t.Fatal("typing a repo's name should make 'Use this folder' select it")
	}
	if r, ok := cmd().(folderPickerResultMsg); !ok || r.dir != repo {
		t.Fatalf("expected result dir %q, got %#v", repo, cmd())
	}
}

// The typed text appears as the pending segment in the breadcrumb (there is
// no separate filter line).
func TestFolderPicker_BreadcrumbShowsTypedSegment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fp := newFolderPicker(root)
	fp.applyQuery("foobar")

	bc := strings.TrimRight(ansi.Strip(fp.renderBreadcrumb(120)), "█")
	if !strings.HasSuffix(bc, "/foobar") {
		t.Fatalf("expected breadcrumb to end with the typed segment '/foobar', got %q", bc)
	}
}

// Typing an absolute path navigates to the deepest existing directory and
// filters the current level by the trailing segment.
func TestFolderPicker_TypingPathNavigates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sub := mkSubdir(t, root, "nested")
	mkSubdir(t, sub, "deep")

	fp := newFolderPicker(root)
	fp.applyQuery(sub + "/de")

	if fp.current != sub {
		t.Fatalf("expected to navigate to %q, got %q", sub, fp.current)
	}
	found := false
	for _, it := range fp.items {
		if it.kind == folderItemSubdir && it.name == "deep" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected 'deep' to survive the 'de' filter after navigating")
	}
}

// The breadcrumb shows the remembered deeper path (the dimmed trail) once
// you've descended and come back up.
func TestFolderPicker_BreadcrumbIncludesMemorizedTrail(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mid := mkSubdir(t, root, "mid")
	mkSubdir(t, mid, "leaf")

	fp := newFolderPicker(root)
	descend := func(name string) {
		for i, it := range fp.items {
			if it.kind == folderItemSubdir && it.name == name {
				fp.cursor = i
			}
		}
		fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	descend("mid")
	descend("leaf")
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // back to mid
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // back to root

	if trail := fp.memorizedTrail(); len(trail) == 0 || trail[0] != "mid" {
		t.Fatalf("expected memorized trail starting with 'mid', got %v", trail)
	}
	if bc := ansi.Strip(fp.renderBreadcrumb(80)); !strings.Contains(bc, "mid") {
		t.Fatalf("expected breadcrumb to include the dimmed trail, got %q", bc)
	}
}

// Regression: the FIRST ← from a folder the picker opened directly into must
// land on that folder in the parent — even without a prior descent.
func TestFolderPicker_FirstUpLandsOnOriginFolder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := mkSubdir(t, root, "child")
	mkSubdir(t, root, "sibling")

	fp := newFolderPicker(child) // opened directly inside "child"
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if fp.current != root {
		t.Fatalf("expected to go up to %q, got %q", root, fp.current)
	}
	if it := fp.cursorItem(); it.kind != folderItemSubdir || it.name != "child" {
		t.Fatalf("expected cursor on 'child' after first ←, got %+v", it)
	}
}

// Regression: "~Downloads" must not expand to homeDownloads — tilde expansion
// requires exactly "~" or "~/" prefix.
func TestFolderPicker_TildeWithoutSlashNotExpanded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fp := newFolderPicker(root)
	initial := fp.current
	fp.applyQuery("~Downloads")
	if fp.current != initial {
		t.Fatalf("~Downloads must not navigate away from %q, got %q", initial, fp.current)
	}
}

// Regression: "/" with an empty search must pass through to the text input so
// the user can type absolute paths — descendIntoMatch must not intercept it.
func TestFolderPicker_SlashPassesThroughWhenSearchEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fp := newFolderPicker(root)
	fp, _ = fp.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if !strings.HasPrefix(fp.search.Value(), "/") {
		t.Fatalf("'/' with empty search must reach text input, got %q", fp.search.Value())
	}
}

// Regression: truncateLeft must not panic when the path contains wide chars
// (emoji/CJK) where visual width > rune count.
func TestTruncateLeft_NoPanicOnWideChars(t *testing.T) {
	t.Parallel()
	// 3 emoji = visual width 6; budget 5 previously caused a negative slice index.
	got := truncateLeft("🌟🌟🌟", 5)
	if got == "" {
		t.Fatal("expected non-empty truncation result")
	}
}

// Navigating into a directory and back out lands the cursor on the folder
// you came from, so a ←/→ round-trip is a clean no-op.
func TestFolderPicker_RemembersCursorAcrossUpDown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkSubdir(t, root, "aaa")
	target := mkSubdir(t, root, "bbb")
	mkSubdir(t, root, "ccc")
	mkSubdir(t, target, "inner")

	fp := newFolderPicker(root)
	for i, it := range fp.items {
		if it.kind == folderItemSubdir && it.name == "bbb" {
			fp.cursor = i
		}
	}

	// → into bbb, then ← back: the cursor returns to "bbb".
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if fp.current != target {
		t.Fatalf("expected to descend into %q, got %q", target, fp.current)
	}
	fp, _ = fp.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if it := fp.cursorItem(); it.kind != folderItemSubdir || it.name != "bbb" {
		t.Fatalf("expected cursor to return to 'bbb', got %+v", it)
	}
}
