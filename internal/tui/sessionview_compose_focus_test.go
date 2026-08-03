package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/supaclank/clank/internal/agent"
)

// composeKey drives one key press through the compose Update path and
// returns the updated model.
func composeKey(t *testing.T, m *SessionViewModel, msg tea.KeyPressMsg) *SessionViewModel {
	t.Helper()
	model, _ := m.Update(msg)
	return model.(*SessionViewModel)
}

func newComposeForFocusTest(t *testing.T) *SessionViewModel {
	t.Helper()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40
	return m
}

// focusRow presses Up until the given row is focused, so tests don't hard-code
// the number of rows between the prompt and a target field.
func focusRow(t *testing.T, m *SessionViewModel, target composeFocus) *SessionViewModel {
	t.Helper()
	for i := 0; i < 8 && m.composeFocus != target; i++ {
		m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	}
	if m.composeFocus != target {
		t.Fatalf("could not focus row %d (stuck at %d)", target, m.composeFocus)
	}
	return m
}

// pinOpenCodeBackend forces a deterministic starting backend so backend-switch
// assertions don't depend on the machine's saved DefaultBackend preference
// (NewSessionViewComposing reads it via config.LoadPreferences).
func pinOpenCodeBackend(m *SessionViewModel) {
	m.backend = agent.BackendOpenCode
	m.modes, m.selectedMode = nil, 0
}

func TestComposeFocus_UpFromPromptWalksRowsAndClamps(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)

	// Fresh compose starts on the prompt with the textarea focused.
	if m.composeFocus != focusPrompt {
		t.Fatalf("expected focusPrompt initially, got %d", m.composeFocus)
	}
	if !m.input.Focused() {
		t.Fatal("expected textarea focused initially")
	}

	// Up from the empty prompt (cursor on line 0) escapes row by row to the
	// top: New worktree → Worktree → Project → Backend, then clamps.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.composeFocus != focusNewWorktree {
		t.Fatalf("expected focusNewWorktree after first up, got %d", m.composeFocus)
	}
	if m.input.Focused() {
		t.Fatal("expected textarea blurred once focus left the prompt")
	}

	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.composeFocus != focusWorktree {
		t.Fatalf("expected focusWorktree after second up, got %d", m.composeFocus)
	}

	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.composeFocus != focusFolder {
		t.Fatalf("expected focusFolder after third up, got %d", m.composeFocus)
	}

	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.composeFocus != focusBackend {
		t.Fatalf("expected focusBackend after fourth up, got %d", m.composeFocus)
	}

	// Up at the top row clamps (no wrap).
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.composeFocus != focusBackend {
		t.Fatalf("expected focus to clamp at focusBackend, got %d", m.composeFocus)
	}
}

func TestComposeFocus_DownReturnsToPrompt(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)

	// Walk up to the backend row.
	m = focusRow(t, m, focusBackend)

	// Down steps back through the rows to the prompt and refocuses it.
	for _, want := range []composeFocus{focusFolder, focusWorktree, focusNewWorktree, focusPrompt} {
		m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
		if m.composeFocus != want {
			t.Fatalf("expected focus %d after down, got %d", want, m.composeFocus)
		}
	}
	if !m.input.Focused() {
		t.Fatal("expected textarea refocused on return to prompt")
	}
}

// When the cursor sits below the first line, Up moves the textarea cursor
// rather than escaping focus upward.
func TestComposeFocus_UpStaysInPromptWhenCursorBelowFirstLine(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)

	m.input.SetValue("line one\nline two\nline three")
	// Move the cursor down so it is no longer on the first line.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.input.Line() == 0 {
		t.Skip("textarea did not advance cursor off the first line; nothing to assert")
	}

	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.composeFocus != focusPrompt {
		t.Fatalf("expected focus to stay on prompt, got %d", m.composeFocus)
	}
	if !m.input.Focused() {
		t.Fatal("expected textarea to remain focused")
	}
}

func TestComposeFocus_BackendSwitchWithArrows(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	pinOpenCodeBackend(m)

	// Focus the backend row.
	m = focusRow(t, m, focusBackend)

	// Right hovers claude-code; Enter commits it.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.backend != agent.BackendOpenCode {
		t.Fatal("hovering with ←/→ must not switch the backend until Enter")
	}
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.backend != agent.BackendClaudeCode {
		t.Fatalf("expected claude-code after right+enter, got %s", m.backend)
	}
	// Left hovers opencode; Enter commits it.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.backend != agent.BackendOpenCode {
		t.Fatalf("expected opencode after left+enter, got %s", m.backend)
	}
}

// Switching backend must drop the previous backend's model list so a stale
// selectedModel index can't be submitted to the wrong CLI.
func TestComposeFocus_BackendSwitchResetsModelSelection(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	pinOpenCodeBackend(m)
	m.models = []agent.ModelInfo{{ID: "x", ProviderID: "p"}, {ID: "y", ProviderID: "p"}}
	m.selectedModel = 1

	// Focus backend, hover claude-code, and commit with Enter.
	m = focusRow(t, m, focusBackend)
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.backend != agent.BackendClaudeCode {
		t.Fatalf("expected claude-code, got %s", m.backend)
	}
	if m.models != nil {
		t.Fatalf("expected models cleared on backend switch, got %d", len(m.models))
	}
	if m.selectedModel != -1 {
		t.Fatalf("expected selectedModel reset to -1, got %d", m.selectedModel)
	}
}

// Re-selecting the already-active backend is a no-op (no model reset / refetch).
func TestComposeFocus_BackendSameSelectionIsNoop(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	pinOpenCodeBackend(m)
	m.models = []agent.ModelInfo{{ID: "x", ProviderID: "p"}}
	m.selectedModel = 0

	m = focusRow(t, m, focusBackend)
	// Hover opencode (already active) and commit — a no-op.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.models == nil || m.selectedModel != 0 {
		t.Fatal("expected re-committing the active backend to leave model state intact")
	}
}

// Enter on the Backend row commits the hovered backend and stays on the row
// (the green active marker shifts) — it must not jump to the prompt or launch.
func TestComposeFocus_EnterOnBackendCommitsAndStays(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	pinOpenCodeBackend(m)
	m.input.SetValue("a prompt that must not be submitted")

	m = focusRow(t, m, focusBackend)
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // hover claude
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // commit

	if m.backend != agent.BackendClaudeCode {
		t.Fatalf("expected enter to commit the hovered backend, got %s", m.backend)
	}
	if m.composeFocus != focusBackend {
		t.Fatalf("expected focus to stay on the backend row, got %d", m.composeFocus)
	}
	if m.submitting {
		t.Fatal("enter on the backend row must not launch a session")
	}
	if m.err != nil {
		t.Fatalf("unexpected error: %v", m.err)
	}
}

// Esc exits compose from any row (not just the prompt) — a single press.
func TestComposeFocus_EscExitsFromAnyRow(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp}) // focus a label row

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("expected a command from esc")
	}
	if _, ok := cmd().(closeComposeMsg); !ok {
		t.Fatalf("expected esc to exit compose (closeComposeMsg), got %T", cmd())
	}
}

// Fields render borderless — one line each — and the height never changes
// with focus (zero layout shift; the chevron swaps with equal-width spaces).
func TestComposeFields_BorderlessFixedHeight(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	fields := []composeFieldSpec{
		{label: "Backend", value: "v1", focus: focusBackend},
		{label: "Project", value: "v2", focus: focusFolder},
	}

	// One line per field, no box-drawing characters.
	plain := ansi.Strip(m.renderComposeFields(fields))
	if got := strings.Count(plain, "\n") + 1; got != len(fields) {
		t.Fatalf("expected %d field lines, got %d:\n%s", len(fields), got, plain)
	}
	if strings.ContainsAny(plain, "╭╮╰╯├┤│─") {
		t.Fatalf("expected no borders; got:\n%s", plain)
	}

	// Height is identical regardless of which row is focused.
	m.composeFocus = focusBackend
	a := strings.Count(m.renderComposeFields(fields), "\n")
	m.composeFocus = focusFolder
	b := strings.Count(m.renderComposeFields(fields), "\n")
	if a != b {
		t.Fatalf("field height changed with focus: %d vs %d", a, b)
	}
}

// Moving focus between rows must not change the overall view height.
func TestComposeFocus_ZeroLayoutShift(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	pinOpenCodeBackend(m)

	height := func() int { return strings.Count(ansi.Strip(m.View().Content), "\n") }
	base := height() // prompt focused

	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp}) // Project
	if got := height(); got != base {
		t.Fatalf("focusing Project shifted layout: %d -> %d lines", base, got)
	}
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp}) // Backend
	if got := height(); got != base {
		t.Fatalf("focusing Backend shifted layout: %d -> %d lines", base, got)
	}
}

// Labels stay on the left in every focus state; a focused row shows the
// vibrant chevron cursor.
func TestComposeFocus_LabelsStayLeftChevronOnFocus(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	pinOpenCodeBackend(m)

	labels := func() bool {
		p := ansi.Strip(m.View().Content)
		return strings.Contains(p, "Backend:") && strings.Contains(p, "Project:")
	}
	hasChevron := func() bool { return strings.Contains(ansi.Strip(m.View().Content), "›") }

	// Prompt focused: labels present, no field chevron.
	if !labels() {
		t.Fatal("expected Backend:/Project: labels while the prompt is focused")
	}
	if hasChevron() {
		t.Fatal("did not expect a field chevron while the prompt is focused")
	}

	// Focusing a field keeps the labels and shows the chevron cursor.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if !labels() {
		t.Fatal("expected labels to remain on the left when a field is focused")
	}
	if !hasChevron() {
		t.Fatal("expected a vibrant chevron cursor on the focused row")
	}
}

func TestComposeFocus_NewWorktreeToggleFlips(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	if m.isNewWorktree {
		t.Fatal("expected new-worktree off by default")
	}

	// Up once from the prompt focuses the New-worktree row.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.composeFocus != focusNewWorktree {
		t.Fatalf("expected focusNewWorktree, got %d", m.composeFocus)
	}

	// Enter flips it on, then off, staying on the row.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.isNewWorktree {
		t.Fatal("expected new-worktree enabled after enter")
	}
	if m.composeFocus != focusNewWorktree {
		t.Fatal("expected focus to stay on the toggle after flipping")
	}
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.isNewWorktree {
		t.Fatal("expected new-worktree disabled after a second enter")
	}
}

// Regression for the Shift+N pain point: the new-worktree flag must reach
// the compose view. isShiftN detection (above) plus this wiring is the full
// path from key press to a pre-enabled toggle.
func TestInbox_OpenComposingSessionThreadsNewWorktree(t *testing.T) {
	t.Parallel()

	withFlag := &InboxModel{}
	_ = withFlag.openComposingSession("/tmp/x", true)
	if withFlag.sessionView == nil || !withFlag.sessionView.isNewWorktree {
		t.Fatal("Shift+N path should open compose with new-worktree enabled")
	}

	plain := &InboxModel{}
	_ = plain.openComposingSession("/tmp/x", false)
	if plain.sessionView == nil || plain.sessionView.isNewWorktree {
		t.Fatal("plain n path should open compose with new-worktree off")
	}
}

// Bracket SHAPE tracks selection (tight = active), not the cursor: the
// active backend clamps tight "[x]" and the other sits open "[ x ]".
// Committing the other with Enter swaps the clamp.
func TestComposeFocus_BackendBracketsTightWhenSelected(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	pinOpenCodeBackend(m) // opencode active
	m = focusRow(t, m, focusBackend)

	v := ansi.Strip(m.backendValue(true))
	if !strings.Contains(v, "[OpenCode]") {
		t.Fatalf("expected the selected backend tight as '[OpenCode]', got %q", v)
	}
	if !strings.Contains(v, "[ Claude Code ]") {
		t.Fatalf("expected the unselected backend open as '[ Claude Code ]', got %q", v)
	}

	// Moving the cursor (without committing) must NOT change the shapes.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	v = ansi.Strip(m.backendValue(true))
	if !strings.Contains(v, "[OpenCode]") || !strings.Contains(v, "[ Claude Code ]") {
		t.Fatalf("hovering must not change bracket shape (only color); got %q", v)
	}

	// Commit claude → the tight clamp swaps to it; opencode opens.
	m = composeKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	v = ansi.Strip(m.backendValue(true))
	if !strings.Contains(v, "[Claude Code]") || !strings.Contains(v, "[ OpenCode ]") {
		t.Fatalf("expected claude tight + opencode open after commit, got %q", v)
	}
}

// "q" on a label row quits (matching the inbox) and must not open the folder
// picker via the Project row's type-to-open behavior.
func TestComposeFocus_QQuitsOnLabelRow(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	m = focusRow(t, m, focusFolder)

	model, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	m = model.(*SessionViewModel)
	if m.showFolderPicker {
		t.Fatal("q on a label row must not open the folder picker")
	}
	if cmd == nil {
		t.Fatal("expected a command from q")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg from q on a label row, got %T", cmd())
	}
}

func TestIsShiftN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  tea.KeyPressMsg
		want bool
	}{
		{"uppercase text (warp/legacy, no modifier)", tea.KeyPressMsg{Text: "N"}, true},
		{"modshift+n (kitty protocol)", tea.KeyPressMsg{Code: 'n', Mod: tea.ModShift}, true},
		{"modshift+N", tea.KeyPressMsg{Code: 'N', Mod: tea.ModShift}, true},
		{"plain n", tea.KeyPressMsg{Code: 'n', Text: "n"}, false},
		{"other uppercase letter", tea.KeyPressMsg{Text: "M"}, false},
	}
	for _, c := range cases {
		if got := isShiftN(c.msg); got != c.want {
			t.Errorf("%s: isShiftN = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCompose_EffectiveWorktreeBranch(t *testing.T) {
	t.Parallel()

	// New-worktree on with no explicit branch → a fresh name is minted.
	on := newComposeForFocusTest(t)
	on.isNewWorktree = true
	if on.effectiveWorktreeBranch() == "" {
		t.Fatal("expected a generated branch when new-worktree is on")
	}

	// New-worktree off → no branch (run in the current worktree).
	off := newComposeForFocusTest(t)
	if got := off.effectiveWorktreeBranch(); got != "" {
		t.Fatalf("expected empty branch when new-worktree off, got %q", got)
	}

	// An explicit branch is preserved as-is.
	explicit := newComposeForFocusTest(t)
	explicit.isNewWorktree = true
	explicit.worktreeBranch = "feat/x"
	if got := explicit.effectiveWorktreeBranch(); got != "feat/x" {
		t.Fatalf("expected explicit branch preserved, got %q", got)
	}
}

// Regression: in composing mode the prompt box reserves its badge line even
// when empty, so toggling backend (badge present for claude, absent for a
// freshly-switched opencode) doesn't resize the box and jump the layout.
func TestComposePromptBox_ReservesBadgeLineForZeroJump(t *testing.T) {
	t.Parallel()
	m := newComposeForFocusTest(t)
	m.composing = true

	m.modes, m.selectedMode = nil, 0
	withoutBadge := strings.Count(m.renderPromptBox(), "\n")

	m.modes, m.selectedMode = claudePermissionModes()
	withBadge := strings.Count(m.renderPromptBox(), "\n")

	if withoutBadge != withBadge {
		t.Fatalf("prompt box height changed with badge presence (%d vs %d) — jumps on backend toggle", withoutBadge, withBadge)
	}
}
