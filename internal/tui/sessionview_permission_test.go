package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
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

// modelsResultMsg always follows the sessionInfoMsg seeding block (every
// backend branch there returns a fetchModels() command), so the agent's
// current model must be re-derived from m.info here — a bare "reset to -1"
// silently discards the active-model marking on every session open.
func TestSessionView_ModelsResultMsg_PreservesAgentCurrentModel(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "sess-1")
	m.width, m.height = 80, 40
	m.info = &agent.SessionInfo{CurrentModelID: "opus"}

	model, _ := m.Update(modelsResultMsg{models: []agent.ModelInfo{
		{ID: "sonnet"}, {ID: "opus"},
	}})
	m = model.(*SessionViewModel)

	if m.selectedModel != 1 {
		t.Fatalf("selectedModel = %d, want 1 (opus, the agent's current model)", m.selectedModel)
	}
}

// A live session already runs a model (info.CurrentModelID); a saved
// per-backend preference must NOT override it. Otherwise reopening a
// session running opus would highlight the user's global pref (sonnet)
// and the next send would silently switch the running session's model.
func TestSessionView_ModelsResultMsg_CurrentModelWinsOverPref(t *testing.T) {
	// Not t.Parallel: CLANK_DIR is process-global.
	t.Setenv("CLANK_DIR", t.TempDir())
	if err := config.SavePreferences(config.Preferences{}); err != nil {
		t.Fatalf("SavePreferences: %v", err)
	}
	prefs, err := config.LoadPreferences()
	if err != nil {
		t.Fatalf("LoadPreferences: %v", err)
	}
	prefs.SetModelFor(string(agent.BackendClaudeCode), config.ModelPreference{ModelID: "sonnet", ProviderID: "anthropic"})
	if err := config.SavePreferences(prefs); err != nil {
		t.Fatalf("SavePreferences: %v", err)
	}

	m := NewSessionViewModel(nil, "sess-1")
	m.width, m.height = 80, 40
	m.backend = agent.BackendClaudeCode
	m.info = &agent.SessionInfo{Backend: agent.BackendClaudeCode, CurrentModelID: "opus"}

	model, _ := m.Update(modelsResultMsg{models: []agent.ModelInfo{
		{ID: "sonnet", ProviderID: "anthropic"},
		{ID: "opus", ProviderID: "anthropic"},
	}})
	m = model.(*SessionViewModel)

	if m.selectedModel != 1 {
		t.Fatalf("selectedModel = %d, want 1 (opus, the session's current model, not the sonnet pref)", m.selectedModel)
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

// batchYields reports whether cmd (possibly a tea.Batch) produces a
// message of type T when run. Compose models built with an empty
// projectDir short-circuit their fetches to result messages, so the batch
// runs in-process with no client.
func batchYields[T tea.Msg](t *testing.T, cmd tea.Cmd) bool {
	t.Helper()
	if cmd == nil {
		return false
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if batchYields[T](t, sub) {
				return true
			}
		}
		return false
	}
	_, ok := msg.(T)
	return ok
}

// Opening compose ("n" from the chat view) must fetch the agent's modes.
// Only the backend-CHANGE path did, so the mode row stayed empty until the
// user cycled backends with ctrl+b and came back.
func TestCompose_InitFetchesModes(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "", agent.BackendCodex)
	m.width, m.height = 80, 40

	if !batchYields[modesResultMsg](t, m.Init()) {
		t.Fatal("compose Init dispatched no modes fetch — the mode picker stays empty on open")
	}
}

// Modes and models are project-scoped, so switching the compose folder has
// to drop the old list and refetch rather than carry one project's agents
// into another.
func TestCompose_FolderChangeRefetchesModes(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "", agent.BackendOpenCode)
	m.width, m.height = 80, 40

	model, _ := m.Update(modesResultMsg{modes: []agent.SessionMode{{ID: "build", Name: "Build"}}})
	m = model.(*SessionViewModel)
	if len(m.modes) != 1 {
		t.Fatalf("setup: modes = %d, want 1", len(m.modes))
	}

	cmd := m.applyProjectFolder("", focusFolder)
	if len(m.modes) != 0 {
		t.Errorf("previous folder's modes survived the switch: %+v", m.modes)
	}
	if !batchYields[modesResultMsg](t, cmd) {
		t.Fatal("folder switch dispatched no modes fetch")
	}
}

// The prewarmed picker shows instantly; a background refine re-fetches once
// so a per-dir backend's per-repo agents surface a beat later.
func TestCompose_RefineReFetchesModesAndModels(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "", agent.BackendOpenCode)
	m.width, m.height = 80, 40

	model, cmd := m.Update(refineCatalogMsg{})
	m = model.(*SessionViewModel)
	if !batchYields[modesResultMsg](t, cmd) {
		t.Error("refine dispatched no modes re-fetch")
	}
	if !batchYields[modelsResultMsg](t, cmd) {
		t.Error("refine dispatched no models re-fetch")
	}
}

// A refine must not reset the user's mode pick: if the refreshed list still
// offers it, the selection follows it by id (its index may shift as
// per-repo agents are appended).
func TestCompose_RefinePreservesSelectedMode(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendOpenCode)
	m.width, m.height = 80, 40

	model, _ := m.Update(modesResultMsg{modes: []agent.SessionMode{
		{ID: "build", Name: "Build"}, {ID: "plan", Name: "Plan"},
	}})
	m = model.(*SessionViewModel)
	m.selectedMode = 1 // user picks "plan"

	// Refine lands: same built-ins plus a per-repo agent, reordered.
	model, _ = m.Update(modesResultMsg{modes: []agent.SessionMode{
		{ID: "reviewer", Name: "Reviewer"}, {ID: "build", Name: "Build"}, {ID: "plan", Name: "Plan"},
	}})
	m = model.(*SessionViewModel)
	if got := string(m.modes[m.selectedMode].perm); got != "plan" {
		t.Fatalf("selected mode = %q after refine, want plan preserved across reorder", got)
	}

	// If the refreshed list drops the pick, selection resets to the first.
	model, _ = m.Update(modesResultMsg{modes: []agent.SessionMode{
		{ID: "build", Name: "Build"},
	}})
	m = model.(*SessionViewModel)
	if m.selectedMode != 0 {
		t.Fatalf("selectedMode = %d after pick vanished, want 0", m.selectedMode)
	}
}
