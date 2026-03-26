package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/acksell/clank/internal/agent"
)

func TestNewSessionModel_BackendToggle(t *testing.T) {
	t.Parallel()
	m := newNewSessionModel("/tmp/project")
	m.width = 80
	m.height = 40

	// Start on prompt field, tab to backend field.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != fieldBackend {
		t.Fatalf("expected focus on backend, got %d", m.focus)
	}

	// Default backend is opencode.
	if m.backend != agent.BackendOpenCode {
		t.Fatalf("expected default backend opencode, got %s", m.backend)
	}

	// Toggle with space.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if m.backend != agent.BackendClaudeCode {
		t.Fatalf("expected claude-code after toggle, got %s", m.backend)
	}

	// Toggle back with left arrow.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if m.backend != agent.BackendOpenCode {
		t.Fatalf("expected opencode after second toggle, got %s", m.backend)
	}
}

func TestNewSessionModel_LaunchEmitsMsg(t *testing.T) {
	t.Parallel()
	m := newNewSessionModel("/tmp/project")
	m.width = 80
	m.height = 40

	// Type a prompt. Focus starts on prompt field.
	m.prompt.SetValue("fix the auth bug")

	// Launch with ctrl+l.
	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})

	if cmd == nil {
		t.Fatal("expected a command from launch")
	}

	msg := cmd()
	launch, ok := msg.(newSessionLaunchMsg)
	if !ok {
		t.Fatalf("expected newSessionLaunchMsg, got %T", msg)
	}
	if launch.req.Backend != agent.BackendOpenCode {
		t.Errorf("expected backend opencode, got %s", launch.req.Backend)
	}
	if launch.req.Prompt != "fix the auth bug" {
		t.Errorf("expected prompt 'fix the auth bug', got %q", launch.req.Prompt)
	}
	if launch.req.ProjectDir != "/tmp/project" {
		t.Errorf("expected project /tmp/project, got %q", launch.req.ProjectDir)
	}
}

func TestNewSessionModel_LaunchRequiresPrompt(t *testing.T) {
	t.Parallel()
	m := newNewSessionModel("/tmp/project")
	m.width = 80
	m.height = 40

	// Try to launch without typing a prompt.
	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})

	if cmd != nil {
		t.Fatal("expected no command when prompt is empty")
	}
	if m.err == nil {
		t.Fatal("expected error when prompt is empty")
	}
}

func TestNewSessionModel_CancelEmitsMsg(t *testing.T) {
	t.Parallel()
	m := newNewSessionModel("/tmp/project")
	m.width = 80
	m.height = 40

	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if cmd == nil {
		t.Fatal("expected a command from cancel")
	}

	msg := cmd()
	if _, ok := msg.(newSessionCancelMsg); !ok {
		t.Fatalf("expected newSessionCancelMsg, got %T", msg)
	}
}

func TestNewSessionModel_TabCycles(t *testing.T) {
	t.Parallel()
	m := newNewSessionModel("/tmp/project")
	m.width = 80
	m.height = 40

	// Focus starts on prompt.
	if m.focus != fieldPrompt {
		t.Fatalf("expected initial focus on prompt, got %d", m.focus)
	}

	// Tab cycles: prompt -> backend -> project -> prompt.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != fieldBackend {
		t.Fatalf("after 1 tab: expected backend, got %d", m.focus)
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != fieldProject {
		t.Fatalf("after 2 tabs: expected project, got %d", m.focus)
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != fieldPrompt {
		t.Fatalf("after 3 tabs: expected prompt, got %d", m.focus)
	}
}

func TestNewSessionModel_ViewRenders(t *testing.T) {
	t.Parallel()
	m := newNewSessionModel("/tmp/project")
	m.width = 80
	m.height = 40

	view := m.View()
	if view == "" {
		t.Fatal("expected non-empty view")
	}
	if view == "Loading..." {
		t.Fatal("expected rendered view, got Loading...")
	}
}
