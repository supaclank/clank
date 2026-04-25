package tui

import (
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
)

// TestNextDefaultBackend_CyclesAndPersists verifies the settings-page
// cycle logic: each call returns the next backend in agent.AllBackends
// and persistDefaultBackend round-trips through preferences.json.
//
// Drives both helpers because they're a tightly coupled pair — the UI
// feeds the in-memory current value into nextDefaultBackend and the
// resolved next is persisted by persistDefaultBackend.
func TestNextDefaultBackend_CyclesAndPersists(t *testing.T) {
	// Not t.Parallel: $HOME is process-global; LoadPreferences/Save
	// would race with sibling config tests if we parallelised.
	t.Setenv("HOME", t.TempDir())

	// First call: empty in-memory value → current resolves to default
	// (opencode), so next should be claude-code.
	got := nextDefaultBackend("")
	if got != agent.BackendClaudeCode {
		t.Fatalf("first cycle: got %q, want claude-code", got)
	}
	persistDefaultBackend(got)

	// Second call: current is claude-code → cycle wraps to opencode.
	got = nextDefaultBackend(string(agent.BackendClaudeCode))
	if got != agent.BackendOpenCode {
		t.Fatalf("wrap cycle: got %q, want opencode", got)
	}
	persistDefaultBackend(got)

	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	if prefs.DefaultBackend != string(agent.BackendOpenCode) {
		t.Errorf("persisted DefaultBackend: got %q, want opencode", prefs.DefaultBackend)
	}
}
