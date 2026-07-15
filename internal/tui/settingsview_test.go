package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

func TestSettingsView_NewShowsCurrentScheme(t *testing.T) {
	t.Parallel()

	s := newSettingsView("dracula", "")
	if s.entries[0].kind != settingsEntryColorScheme {
		t.Fatalf("expected first entry to be color scheme, got %v", s.entries[0].kind)
	}
	if s.entries[0].value != "dracula" {
		t.Errorf("expected value 'dracula', got %q", s.entries[0].value)
	}
}

// TestSettingsView_NewShowsCurrentDefaultBackend pins the default-backend
// row's wiring — adding/reordering entries should be a deliberate change.
func TestSettingsView_NewShowsCurrentDefaultBackend(t *testing.T) {
	t.Parallel()

	s := newSettingsView("", "claude-code")
	if s.entries[1].kind != settingsEntryDefaultBackend {
		t.Fatalf("expected second entry to be default backend, got %v", s.entries[1].kind)
	}
	if s.entries[1].value != "claude-code" {
		t.Errorf("expected value 'claude-code', got %q", s.entries[1].value)
	}
}

// TestSettingsView_DefaultBackend_EmptyShowsBuiltInDefault verifies an
// unset preference renders as the built-in default backend.
func TestSettingsView_DefaultBackend_EmptyShowsBuiltInDefault(t *testing.T) {
	t.Parallel()

	s := newSettingsView("", "")
	if got, want := s.entries[1].value, string(agent.DefaultBackend); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSettingsView_SetDefaultBackendValueUpdatesEntry verifies the value
// column refreshes after the inbox cycles the backend, without rebuilding
// the whole settings view.
func TestSettingsView_SetDefaultBackendValueUpdatesEntry(t *testing.T) {
	t.Parallel()

	s := newSettingsView("", "opencode")
	s.SetDefaultBackendValue("claude-code")
	if got := s.entries[1].value; got != "claude-code" {
		t.Errorf("entry value: got %q, want claude-code", got)
	}

	// Empty resolves to the built-in default.
	s.SetDefaultBackendValue("")
	if got, want := s.entries[1].value, string(agent.DefaultBackend); got != want {
		t.Errorf("empty should resolve to %q, got %q", want, got)
	}
}

func TestSettingsView_NewEmptySchemeDisplaysDefault(t *testing.T) {
	t.Parallel()

	s := newSettingsView("", "")
	if got, want := s.entries[0].value, builtInSchemes[0].Name; got != want {
		t.Errorf("empty scheme name should resolve to %q, got %q", want, got)
	}
}

// TestSettingsView_EnterEmitsActivatedMsg verifies the page emits a message
// for the inbox to open the scheme picker, rather than opening it itself
// (keeping overlay ownership in the inbox).
func TestSettingsView_EnterEmitsActivatedMsg(t *testing.T) {
	t.Parallel()

	s := newSettingsView("", "")
	s, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected Update to return a cmd on Enter")
	}
	msg := cmd()
	act, ok := msg.(settingsActivatedMsg)
	if !ok {
		t.Fatalf("expected settingsActivatedMsg, got %T", msg)
	}
	if act.kind != settingsEntryColorScheme {
		t.Errorf("kind: got %v, want settingsEntryColorScheme", act.kind)
	}
	_ = s
}

func TestSettingsView_EscEmitsCloseMsg(t *testing.T) {
	t.Parallel()

	s := newSettingsView("", "")
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected cmd on esc")
	}
	msg := cmd()
	if _, ok := msg.(settingsCloseMsg); !ok {
		t.Errorf("expected settingsCloseMsg, got %T", msg)
	}
}

func TestSettingsView_LeftEmitsFocusSidebarMsg(t *testing.T) {
	t.Parallel()

	s := newSettingsView("", "")
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if cmd == nil {
		t.Fatal("expected cmd on left")
	}
	msg := cmd()
	if _, ok := msg.(settingsFocusSidebarMsg); !ok {
		t.Errorf("expected settingsFocusSidebarMsg, got %T", msg)
	}
}

func TestSettingsView_SetColorSchemeValueUpdatesEntry(t *testing.T) {
	t.Parallel()

	s := newSettingsView("default", "")
	s.SetColorSchemeValue("tokyo-night")
	if got := s.entries[0].value; got != "tokyo-night" {
		t.Errorf("entry value: got %q, want tokyo-night", got)
	}

	// Empty name resolves to the default built-in scheme.
	s.SetColorSchemeValue("")
	if got, want := s.entries[0].value, builtInSchemes[0].Name; got != want {
		t.Errorf("empty name should resolve to %q, got %q", want, got)
	}
}

func TestSettingsView_ViewContainsLabelAndValue(t *testing.T) {
	t.Parallel()

	s := newSettingsView("gruvbox-dark", "")
	s.SetSize(80, 30)
	s.SetFocused(true)
	out := s.View()
	if !strings.Contains(out, "Change color scheme") {
		t.Errorf("expected entry label in view, got:\n%s", out)
	}
	if !strings.Contains(out, "gruvbox-dark") {
		t.Errorf("expected current value in view, got:\n%s", out)
	}
}
