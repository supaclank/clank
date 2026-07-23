package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
)

func TestClaudePermissionModes_DefaultsToBypass(t *testing.T) {
	t.Parallel()
	modes, sel := claudePermissionModes()
	if len(modes) != len(agent.ClaudePermissionModes) {
		t.Fatalf("got %d modes, want %d", len(modes), len(agent.ClaudePermissionModes))
	}
	if modes[sel].perm != agent.ClaudePermBypass {
		t.Errorf("default selected perm=%q, want bypassPermissions", modes[sel].perm)
	}
	// Every row is a permission row (perm set, agent empty, label present).
	for _, m := range modes {
		if m.perm == "" || m.agent != "" || m.label == "" {
			t.Errorf("malformed claude mode row: %+v", m)
		}
	}
}

func TestAgentSelectableModes_Selection(t *testing.T) {
	t.Parallel()
	agents := []agent.AgentInfo{{Name: "plan"}, {Name: "build"}, {Name: "review"}}

	modes, sel := agentSelectableModes(agents, "")
	if modes[sel].agent != "build" {
		t.Errorf("default selected agent=%q, want build", modes[sel].agent)
	}
	// Agent rows carry no permission mode.
	for _, m := range modes {
		if m.agent == "" || m.perm != "" {
			t.Errorf("malformed agent mode row: %+v", m)
		}
	}

	modes2, sel2 := agentSelectableModes(agents, "review")
	if modes2[sel2].agent != "review" {
		t.Errorf("selected agent=%q, want review (current override)", modes2[sel2].agent)
	}
}

// Toggling to the Claude backend seeds the static permission modes (default
// bypass), and Tab cycles through them, wrapping around.
func TestCompose_ClaudeBackendSeedsAndCyclesModes(t *testing.T) {
	t.Parallel()
	// Pin opencode as the starting backend so the toggle-to-claude step
	// doesn't depend on the developer's saved DefaultBackend preference.
	m := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendOpenCode)
	m.width, m.height = 80, 40

	model, _ := m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = model.(*SessionViewModel)
	if m.backend != agent.BackendClaudeCode {
		t.Fatalf("backend=%s, want claude-code", m.backend)
	}
	// Modes arrive from the host (agent-advertised), not from a seed.
	advertised := make([]agent.SessionMode, 0, len(agent.ClaudePermissionModes))
	for _, pm := range agent.ClaudePermissionModes {
		advertised = append(advertised, agent.SessionMode{ID: string(pm), Name: pm.Label()})
	}
	model, _ = m.Update(modesResultMsg{modes: advertised})
	m = model.(*SessionViewModel)
	if len(m.modes) != len(agent.ClaudePermissionModes) {
		t.Fatalf("modes len=%d, want %d", len(m.modes), len(agent.ClaudePermissionModes))
	}

	start := m.selectedMode
	model, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = model.(*SessionViewModel)
	if m.selectedMode == start {
		t.Fatal("Tab did not advance selectedMode")
	}

	// Completing the cycle returns to the starting selection.
	for range len(m.modes) - 1 {
		model, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		m = model.(*SessionViewModel)
	}
	if m.selectedMode != start {
		t.Fatalf("after full cycle selectedMode=%d, want %d", m.selectedMode, start)
	}
}

// Toggling back to OpenCode clears the Claude modes so the agent list refetches.
func TestCompose_ToggleBackToOpenCodeClearsModes(t *testing.T) {
	t.Parallel()
	// Pin opencode as the starting backend so the toggle sequence doesn't
	// depend on the developer's saved DefaultBackend preference.
	m := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendOpenCode)
	m.width, m.height = 80, 40

	model, _ := m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = model.(*SessionViewModel)
	model, _ = m.Update(modesResultMsg{modes: []agent.SessionMode{{ID: "plan", Name: "Plan"}}})
	m = model.(*SessionViewModel)
	if len(m.modes) == 0 {
		t.Fatal("expected fetched modes to populate the picker")
	}

	// Next in the cycle is codex: an ACP-served backend whose modes are
	// agent-owned and arrive in-session — the compose picker clears them.
	model, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = model.(*SessionViewModel)
	if m.backend != agent.BackendCodex {
		t.Fatalf("backend=%s, want codex", m.backend)
	}
	if len(m.modes) != 0 {
		t.Fatalf("expected modes cleared on toggle to codex, got %d", len(m.modes))
	}

	model, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = model.(*SessionViewModel)
	if m.backend != agent.BackendOpenCode {
		t.Fatalf("backend=%s, want opencode", m.backend)
	}
	if len(m.modes) != 0 {
		t.Fatalf("expected modes cleared on toggle to opencode, got %d", len(m.modes))
	}
}

// When an existing Claude session is opened from the inbox (sessionInfoMsg with
// BackendClaudeCode), the model must also dispatch fetchModels so model overrides
// remain available — matching the compose path behaviour.
func TestSessionView_InboxClaudeBackend_FetchesModels(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "sess-1")
	m.width, m.height = 80, 40

	info := &agent.SessionInfo{
		Backend: agent.BackendClaudeCode,
		GitRef:  agent.GitRef{LocalPath: "/tmp/project"},
	}
	model, cmd := m.Update(sessionInfoMsg{info: info})
	m = model.(*SessionViewModel)

	// No runtime modes on this info (dead session): the view must FETCH
	// the agent's list rather than seeding a hardcoded one.
	if len(m.modes) != 0 {
		t.Fatalf("modes len=%d, want 0 until the host answers", len(m.modes))
	}
	if cmd == nil {
		t.Fatal("expected fetch commands from inbox Claude session, got nil")
	}
}

// In an active (non-compose) session, Tab cycles the seeded modes while input
// is active.
func TestSessionView_ActiveTabCyclesModes(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "sess-1")
	m.width, m.height = 80, 40
	m.inputActive = true
	m.modes, m.selectedMode = claudePermissionModes()

	start := m.selectedMode
	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = model.(*SessionViewModel)
	if m.selectedMode == start {
		t.Fatal("Tab did not advance selectedMode in active session")
	}
	if m.modes[m.selectedMode].perm == "" {
		t.Error("selected mode lost its permission value after cycling")
	}
}

// The compose view has no session, so its mode picker can only come from
// the host's agent-advertised list. It previously seeded a hardcoded
// claude list here (stale: no auto/dontAsk) and showed nothing at all for
// other backends. Compose routes messages through updateCompose, which
// has its own switch — a handler on the main Update path never fires
// while composing, which is how this stayed broken after the in-session
// picker was fixed.
func TestCompose_ModesComeFromTheHostNotAHardcodedList(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendCodex)
	m.width, m.height = 80, 40

	if len(m.modes) != 0 {
		t.Fatalf("compose seeded %d modes before the host answered", len(m.modes))
	}

	// Codex's own vocabulary — nothing a hardcoded claude list contains.
	model, _ := m.Update(modesResultMsg{modes: []agent.SessionMode{
		{ID: "read-only", Name: "Read Only"},
		{ID: "agent", Name: "Agent"},
		{ID: "agent-full-access", Name: "Full Access"},
	}})
	m = model.(*SessionViewModel)

	if len(m.modes) != 3 {
		t.Fatalf("compose modes len=%d, want 3 from the host", len(m.modes))
	}
	if m.modes[0].perm != "read-only" || m.modes[2].label != "Full Access" {
		t.Errorf("compose rendered %+v, want the agent's advertised modes verbatim", m.modes)
	}
}
