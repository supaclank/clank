package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

func TestCompose_BackendToggle(t *testing.T) {
	t.Parallel()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40

	// Default backend is opencode.
	if m.backend != agent.BackendOpenCode {
		t.Fatalf("expected default backend opencode, got %s", m.backend)
	}

	// Toggle with ctrl+b.
	model, _ := m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = model.(*SessionViewModel)
	if m.backend != agent.BackendClaudeCode {
		t.Fatalf("expected claude-code after toggle, got %s", m.backend)
	}

	// Toggle back.
	model, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = model.(*SessionViewModel)
	if m.backend != agent.BackendOpenCode {
		t.Fatalf("expected opencode after second toggle, got %s", m.backend)
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
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40

	// Set a prompt value.
	m.input.SetValue("fix the auth bug")

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

func TestCompose_EscStandaloneQuits(t *testing.T) {
	t.Parallel()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40
	m.SetStandalone(true)

	model, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = model.(*SessionViewModel)

	if cmd == nil {
		t.Fatal("expected a command from esc")
	}
	// In standalone mode, esc should quit.
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestCompose_EscNonStandaloneGoesBack(t *testing.T) {
	t.Parallel()
	m := NewSessionViewComposing(nil, "/tmp/project")
	m.width = 80
	m.height = 40
	// standalone is false by default.

	model, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = model.(*SessionViewModel)

	if cmd == nil {
		t.Fatal("expected a command from esc")
	}
	msg := cmd()
	if _, ok := msg.(backToInboxMsg); !ok {
		t.Fatalf("expected backToInboxMsg, got %T", msg)
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
