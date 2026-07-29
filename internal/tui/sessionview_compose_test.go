package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/presets"
	"github.com/acksell/clank/internal/config"
)

// initGitRepoForCompose creates a real git repo with an "origin" remote
// so the compose view's launchSession can resolve the repo identity.
func initGitRepoForCompose(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "T")
	run("git", "remote", "add", "origin", "git@github.com:acksell/clank.git")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "initial")
	return dir
}

func TestCompose_BackendToggle(t *testing.T) {
	t.Parallel()
	// Pin the backend so the toggle assertions don't depend on the
	// developer's saved DefaultBackend preference.
	m := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendOpenCode)
	m.width = 80
	m.height = 40

	// Backend starts as the pinned opencode.
	if m.backend != agent.BackendOpenCode {
		t.Fatalf("expected default backend opencode, got %s", m.backend)
	}

	// Toggle with ctrl+b.
	model, _ := m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = model.(*SessionViewModel)
	if m.backend != agent.BackendClaudeCode {
		t.Fatalf("expected claude-code after toggle, got %s", m.backend)
	}

	// Continue through the rest of AllBackends, then wrap to the first.
	for i := 2; i <= len(agent.AllBackends); i++ {
		want := agent.AllBackends[i%len(agent.AllBackends)]
		model, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
		m = model.(*SessionViewModel)
		if m.backend != want {
			t.Fatalf("toggle %d: expected %s, got %s", i, want, m.backend)
		}
	}
}

// TestNewSessionViewComposing_ResolvesDefaultBackendFromPreferences pins the
// production contract that the compose view's initial backend comes from the
// saved DefaultBackend preference, falling back to agent.DefaultBackend when
// unset. Uses a real on-disk preferences file via CLANK_DIR — no mocks — so
// the injection seam (newSessionViewComposingWithBackend) can't silently drift
// from what NewSessionViewComposing actually resolves.
func TestNewSessionViewComposing_ResolvesDefaultBackendFromPreferences(t *testing.T) {
	// Not t.Parallel: CLANK_DIR is process-global.
	t.Setenv("CLANK_DIR", t.TempDir())

	// No preferences file yet → built-in default (opencode), no seeded modes
	// (OpenCode agents arrive asynchronously, not at construction).
	m := NewSessionViewComposing(nil, "/tmp/project")
	if m.backend != agent.BackendOpenCode {
		t.Fatalf("empty prefs: backend=%s, want opencode", m.backend)
	}
	if len(m.modes) != 0 {
		t.Fatalf("empty prefs: expected no seeded modes for opencode, got %d", len(m.modes))
	}

	// A saved claude-code preference resolves to the claude backend. Modes
	// are agent-advertised and fetched (not seeded), so none are present
	// until the host answers.
	if err := config.SavePreferences(config.Preferences{DefaultBackend: string(agent.BackendClaudeCode)}); err != nil {
		t.Fatalf("SavePreferences: %v", err)
	}
	m = NewSessionViewComposing(nil, "/tmp/project")
	if m.backend != agent.BackendClaudeCode {
		t.Fatalf("claude pref: backend=%s, want claude-code", m.backend)
	}
	if len(m.modes) != 0 {
		t.Fatalf("claude pref: modes len=%d, want 0 (modes are fetched, never hardcoded)", len(m.modes))
	}
}

// Switching the compose project folder must drop the previous folder's
// model pick — model options are project-scoped (opencode aggregates
// per-repo providers), and a lingering pick would ride into the new
// folder as a config["model"] the agent there never advertised.
func TestCompose_ApplyProjectFolder_ClearsStaleModel(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "/tmp/old-project", agent.BackendOpenCode)
	m.width, m.height = 80, 40

	// Seed a stale selection from the previous folder.
	m.models = []agent.ModelInfo{{ID: "gpt-5", ProviderID: "openai"}}
	m.selectedModel = 0

	m.applyProjectFolder(t.TempDir(), focusFolder)

	if m.models != nil {
		t.Errorf("models = %+v, want nil after folder switch", m.models)
	}
	if m.selectedModel != -1 {
		t.Errorf("selectedModel = %d, want -1 after folder switch", m.selectedModel)
	}
}

func TestCompose_EnterWithEmptyPromptShowsError(t *testing.T) {
	t.Parallel()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40

	// Enter with empty prompt should show error.
	model, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = model.(*SessionViewModel)

	if cmd != nil {
		t.Fatal("expected no command when prompt is empty")
	}
	if m.err == nil {
		t.Fatal("expected error when prompt is empty")
	}
}

func TestCompose_EnterWithPromptCreatesSession(t *testing.T) {
	t.Parallel()
	dir := initGitRepoForCompose(t)
	m := NewSessionViewComposing(nil, dir)
	m.width = 80
	m.height = 40

	// Set a prompt value.
	m.input.SetValue("fix the auth bug")

	// Presets must load before launchSession will submit (composeConfig()
	// is nil until then — see TestLaunchSession_BlocksUntilPresetsLoad).
	model, _ := m.Update(presetsResultMsg{backend: m.backend, presets: presets.Sandbox()})
	m = model.(*SessionViewModel)

	// Enter should emit a command (the createSessionCmd).
	model, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = model.(*SessionViewModel)

	if cmd == nil {
		t.Fatal("expected a command from enter with prompt")
	}
	if m.err != nil {
		t.Fatalf("unexpected error: %v", m.err)
	}
	// Note: we can't execute the cmd because it requires a real daemon client.
	// But we verified it's non-nil, meaning launchSession passed validation.
}

func TestCompose_EscClosesOverlay(t *testing.T) {
	t.Parallel()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40

	model, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = model.(*SessionViewModel)

	if cmd == nil {
		t.Fatal("expected a command from esc")
	}
	// Compose is an overlay over the prior right-pane state; Esc emits
	// closeComposeMsg so the inbox can restore that state. (The older
	// backToInboxMsg semantics — always land on the inbox — is reserved
	// for explicit flows like mark-done.)
	msg := cmd()
	if _, ok := msg.(closeComposeMsg); !ok {
		t.Fatalf("expected closeComposeMsg, got %T", msg)
	}
}

func TestCompose_ShiftEnterInsertsNewline(t *testing.T) {
	t.Parallel()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40

	// Type some text.
	m.input.SetValue("line one")

	// Shift+Enter should insert a newline (handled by textarea keybinding).
	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	m = model.(*SessionViewModel)

	val := m.input.Value()
	if val == "line one" {
		t.Fatal("expected shift+enter to modify value, but it's unchanged")
	}
	if !strings.Contains(val, "\n") {
		t.Fatalf("expected newline in textarea value, got %q", val)
	}
}

func TestCompose_ViewRenders(t *testing.T) {
	t.Parallel()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40

	v := m.View()
	content := v.Content
	if content == "" {
		t.Fatal("expected non-empty view")
	}
	if content == "Loading..." {
		t.Fatal("expected rendered view, got Loading...")
	}
	// Should contain key elements of the composing view.
	if !strings.Contains(content, "New Session") {
		t.Error("expected 'New Session' header in compose view")
	}
	if !strings.Contains(content, "Backend:") {
		t.Error("expected 'Backend:' label in compose view")
	}
	if !strings.Contains(content, "Project:") {
		t.Error("expected 'Project:' label in compose view")
	}
}

func TestCompose_HandleCreateResult(t *testing.T) {
	t.Parallel()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40

	m.input.SetValue("fix the auth bug")

	// Simulate a successful session creation result.
	ch := make(chan agent.Event, 1)
	msg := sessionCreateResultMsg{
		sessionID: "test-session-123",
		events:    ch,
		cancel:    func() {},
	}

	model, cmd := m.handleCreateResult(msg)
	m = model.(*SessionViewModel)

	// Should have transitioned out of composing mode.
	if m.composing {
		t.Fatal("expected composing=false after create result")
	}
	if m.sessionID != "test-session-123" {
		t.Fatalf("expected sessionID 'test-session-123', got %q", m.sessionID)
	}
	if m.inputActive {
		t.Fatal("expected inputActive=false after create result")
	}
	// Should have the user's prompt as the first entry.
	if len(m.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(m.entries))
	}
	if m.entries[0].content != "fix the auth bug" {
		t.Fatalf("expected entry content 'fix the auth bug', got %q", m.entries[0].content)
	}
	// Should return commands for fetching info + waiting for events.
	if cmd == nil {
		t.Fatal("expected batch command after create result")
	}
}

func TestCompose_HandleCreateResultError(t *testing.T) {
	t.Parallel()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40

	msg := sessionCreateResultMsg{
		err: errTest,
	}

	model, _ := m.handleCreateResult(msg)
	m = model.(*SessionViewModel)

	// Should still be in composing mode.
	if !m.composing {
		t.Fatal("expected composing=true after error")
	}
	if m.err == nil {
		t.Fatal("expected error to be set")
	}
}

// errTest is a sentinel error for testing.
var errTest = &testError{}

type testError struct{}

func (e *testError) Error() string { return "test error" }

// TestCompose_WordBackwardOnEmptyInput is a regression test for an upstream
// bug in bubbles textarea.wordLeft() that causes an infinite loop when the
// cursor is at position (0,0) — i.e. when the input is empty. Without the
// workaround this test hangs forever.
func TestCompose_WordBackwardOnEmptyInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{name: "alt+b", msg: tea.KeyPressMsg{Code: 'b', Mod: tea.ModAlt}},
		{name: "alt+left", msg: tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := NewSessionViewComposing(nil, "/tmp/project")
			m.width = 80
			m.height = 40

			// Must return immediately instead of hanging.
			model, _ := m.Update(tt.msg)
			m = model.(*SessionViewModel)
			if m.input.Value() != "" {
				t.Fatalf("expected empty input, got %q", m.input.Value())
			}
		})
	}
}

// TestModelBadgeLabel_NoDoublePrefix is a regression test: some agents
// (opencode) already advertise provider-qualified value ids in ID, so
// prefixing ProviderID unconditionally produced a duplicated badge like
// "opencode/opencode/big-pickle".
func TestModelBadgeLabel_NoDoublePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model agent.ModelInfo
		want  string
	}{
		{
			name:  "already provider-qualified id is left alone",
			model: agent.ModelInfo{ID: "opencode/big-pickle", ProviderID: "opencode"},
			want:  "opencode/big-pickle",
		},
		{
			name:  "bare id gets the provider prefix",
			model: agent.ModelInfo{ID: "claude-sonnet-5", ProviderID: "github-copilot"},
			want:  "github-copilot/claude-sonnet-5",
		},
		{
			name:  "no provider id: label is the bare id",
			model: agent.ModelInfo{ID: "default"},
			want:  "default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := modelBadgeLabel(tt.model); got != tt.want {
				t.Fatalf("modelBadgeLabel(%+v) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}
