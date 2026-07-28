package tui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/presets"
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

// Runtime SessionInfo is the ONLY in-session model source (the catalog
// endpoints are gone): the seeding block must highlight the agent's
// current model, not reset to -1 and discard the active-model marking.
func TestSessionView_InfoSeedsModelsAndHighlightsCurrent(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "sess-1")
	m.width, m.height = 80, 40

	model, _ := m.Update(sessionInfoMsg{info: &agent.SessionInfo{
		CurrentModelID:  "opus",
		AvailableModels: []agent.ModelInfo{{ID: "sonnet"}, {ID: "opus"}},
	}})
	m = model.(*SessionViewModel)

	if len(m.models) != 2 {
		t.Fatalf("models = %+v, want the runtime list seeded", m.models)
	}
	if m.selectedModel != 1 {
		t.Fatalf("selectedModel = %d, want 1 (opus, the agent's current model)", m.selectedModel)
	}
}

// A session reporting no current model must stay at -1 (no override):
// nothing — not a saved preference, not a guess — may fill in a model
// the running session didn't report. Sending an unrequested override
// would silently switch the session's model on the next send.
func TestSessionView_InfoWithoutCurrentModelSelectsNoOverride(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "sess-1")
	m.width, m.height = 80, 40

	model, _ := m.Update(sessionInfoMsg{info: &agent.SessionInfo{
		AvailableModels: []agent.ModelInfo{{ID: "sonnet"}, {ID: "opus"}},
	}})
	m = model.(*SessionViewModel)

	if m.selectedModel != -1 {
		t.Fatalf("selectedModel = %d, want -1 (no current model reported, no override invented)", m.selectedModel)
	}
}

// A current model absent from its own advertised list (deprecated or
// custom id) also yields no override — but must not be treated as "no
// opinion" either. IndexFunc's -1 covers both; the invariant is simply
// that nothing else fills in.
func TestSessionView_CurrentModelNotInListSelectsNoOverride(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "sess-1")
	m.width, m.height = 80, 40

	model, _ := m.Update(sessionInfoMsg{info: &agent.SessionInfo{
		CurrentModelID:  "some-deprecated-model",
		AvailableModels: []agent.ModelInfo{{ID: "sonnet"}, {ID: "opus"}},
	}})
	m = model.(*SessionViewModel)

	if m.selectedModel >= 0 {
		t.Fatalf("selectedModel = %d, want -1 (current model not displayable, no substitute)", m.selectedModel)
	}
}

// In compose, Tab cycles the host-served presets (Default, Plan, …),
// wrapping around. Presets arrive from the host, never from a seed.
func TestCompose_TabCyclesPresets(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendClaudeCode)
	m.width, m.height = 80, 40

	if len(m.presets) != 0 {
		t.Fatalf("compose seeded %d presets before the host answered", len(m.presets))
	}
	claude := make([]presets.Preset, 0, 2)
	for _, p := range presets.Sandbox() {
		if p.Backend == agent.BackendClaudeCode {
			claude = append(claude, p)
		}
	}
	model, _ := m.Update(presetsResultMsg{backend: agent.BackendClaudeCode, presets: claude})
	m = model.(*SessionViewModel)
	if len(m.presets) != 2 {
		t.Fatalf("presets len=%d, want Default+Plan", len(m.presets))
	}

	start := m.selectedPreset
	model, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = model.(*SessionViewModel)
	if m.selectedPreset == start {
		t.Fatal("Tab did not advance selectedPreset")
	}

	// Completing the cycle returns to the starting selection.
	for range len(m.presets) - 1 {
		model, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		m = model.(*SessionViewModel)
	}
	if m.selectedPreset != start {
		t.Fatalf("after full cycle selectedPreset=%d, want %d", m.selectedPreset, start)
	}
}

// Toggling backends clears the previous backend's presets and model
// options — a stale Default preset carries the wrong required keys, and
// a model value id from one backend is not advertised by another.
func TestCompose_BackendToggleClearsPresetsAndModels(t *testing.T) {
	t.Parallel()
	// Pin the starting backend so the toggle sequence doesn't depend on
	// the developer's saved DefaultBackend preference.
	m := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendOpenCode)
	m.width, m.height = 80, 40

	model, _ := m.Update(presetsResultMsg{backend: agent.BackendOpenCode, presets: presets.Sandbox()})
	m = model.(*SessionViewModel)
	if len(m.presets) == 0 {
		t.Fatal("setup: presets never populated")
	}
	m.models = []agent.ModelInfo{{ID: "opencode/big-pickle"}}
	m.selectedModel = 0

	model, _ = m.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	m = model.(*SessionViewModel)
	if len(m.presets) != 0 || m.selectedPreset != 0 {
		t.Fatalf("presets survived the toggle: %d selected=%d", len(m.presets), m.selectedPreset)
	}
	if len(m.models) != 0 || m.selectedModel != -1 {
		t.Fatalf("model override survived the toggle: %d selected=%d", len(m.models), m.selectedModel)
	}
}

// A dead session opened from the inbox reports no runtime modes/models.
// Nothing is hardcoded in their place; the message fetch rehydrates the
// backend, and its handler must then re-fetch SessionInfo so the runtime
// fields (the only picker source) arrive on the second pass.
func TestSessionView_DeadSessionHealsPickersViaInfoRefetch(t *testing.T) {
	t.Parallel()
	m := NewSessionViewModel(nil, "sess-1")
	m.width, m.height = 80, 40

	info := &agent.SessionInfo{
		Backend: agent.BackendClaudeCode,
		GitRef:  agent.GitRef{LocalPath: "/tmp/project"},
	}
	model, _ := m.Update(sessionInfoMsg{info: info})
	m = model.(*SessionViewModel)
	if len(m.modes) != 0 || len(m.models) != 0 {
		t.Fatalf("dead session seeded modes=%d models=%d, want none (no hardcoded lists)", len(m.modes), len(m.models))
	}

	model, cmd := m.Update(sessionMessagesMsg{})
	m = model.(*SessionViewModel)
	if cmd == nil {
		t.Fatal("message load with empty runtime fields must re-fetch SessionInfo — pickers stay empty forever otherwise")
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

// Opening compose must also fetch presets — composeConfig() stays nil (and
// launchSession refuses to submit) until they load. fetchPresets was defined
// but never wired into any command batch, so every compose session create
// failed the host's 400 config_incomplete check.
func TestCompose_InitFetchesPresets(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "", agent.BackendCodex)
	m.width, m.height = 80, 40

	if !batchYields[presetsResultMsg](t, m.Init()) {
		t.Fatal("compose Init dispatched no presets fetch — composeConfig() can never leave nil")
	}
}

// Presets are backend-scoped (composeConfig matches p.Backend == m.backend),
// so switching backends must drop the old list and refetch, mirroring the
// modes/models pattern.
func TestCompose_BackendSwitchRefetchesPresets(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "", agent.BackendOpenCode)
	m.width, m.height = 80, 40

	model, _ := m.Update(presetsResultMsg{backend: agent.BackendOpenCode, presets: presets.Sandbox()})
	m = model.(*SessionViewModel)
	if len(m.presets) == 0 {
		t.Fatalf("setup: presets = %d, want > 0", len(m.presets))
	}

	cmd := m.applyBackend(agent.BackendClaudeCode)
	if len(m.presets) != 0 {
		t.Errorf("previous backend's presets survived the switch: %+v", m.presets)
	}
	if !batchYields[presetsResultMsg](t, cmd) {
		t.Fatal("backend switch dispatched no presets fetch")
	}
}

// Model options are project-scoped (opencode aggregates per-repo
// providers), so switching the compose folder drops the override —
// carrying a stale value id into another project would send a config the
// new folder's agent never advertised. Presets are host-scoped and stay.
func TestCompose_FolderChangeDropsModelOverrideKeepsPresets(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "", agent.BackendOpenCode)
	m.width, m.height = 80, 40

	model, _ := m.Update(presetsResultMsg{backend: agent.BackendOpenCode, presets: presets.Sandbox()})
	m = model.(*SessionViewModel)
	m.models = []agent.ModelInfo{{ID: "opencode/big-pickle"}}
	m.selectedModel = 0

	_ = m.applyProjectFolder("", focusFolder)
	if len(m.models) != 0 || m.selectedModel != -1 {
		t.Errorf("previous folder's model override survived: %d selected=%d", len(m.models), m.selectedModel)
	}
	if len(m.presets) == 0 {
		t.Error("presets are host-scoped and must survive a folder change")
	}
}

// A preset refetch (backend toggled away and back) must not reset the
// user's pick when the refreshed list still offers it: selection follows
// the preset id, not its index.
func TestCompose_PresetRefetchPreservesSelectionByID(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendClaudeCode)
	m.width, m.height = 80, 40

	claude := make([]presets.Preset, 0, 2)
	for _, p := range presets.Sandbox() {
		if p.Backend == agent.BackendClaudeCode {
			claude = append(claude, p)
		}
	}
	model, _ := m.Update(presetsResultMsg{backend: agent.BackendClaudeCode, presets: claude})
	m = model.(*SessionViewModel)
	m.selectedPreset = 1 // user picks Plan

	// Refetch lands with a user preset prepended-by-the-host after the
	// built-ins; the pick must follow Plan's id to its new index.
	extended := append([]presets.Preset{}, claude...)
	extended = append(extended, presets.Preset{
		ID: "review", Name: "Review", Backend: agent.BackendClaudeCode,
		Config: map[string]string{agent.ConfigOptionMode: "plan", "model": "default", "effort": "max"},
	})
	model, _ = m.Update(presetsResultMsg{backend: agent.BackendClaudeCode, presets: extended})
	m = model.(*SessionViewModel)
	if p := m.composeSelectedPreset(); p == nil || p.Name != "Plan" {
		t.Fatalf("selected preset after refetch = %+v, want Plan preserved by id", p)
	}

	// If the refreshed list drops the pick, selection resets to the first.
	model, _ = m.Update(presetsResultMsg{backend: agent.BackendClaudeCode, presets: claude[:1]})
	m = model.(*SessionViewModel)
	if m.selectedPreset != 0 {
		t.Fatalf("selectedPreset = %d after pick vanished, want 0", m.selectedPreset)
	}
}

// Compose's create config IS the selected preset (the Default preset
// carries every host-required key), plus the model picker's explicit
// override. Before presets load it returns nil — the host then rejects
// the create loudly instead of the session opening in a factory default.
func TestCompose_ConfigComesFromSelectedPresetPlusModelPick(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendClaudeCode)
	m.width, m.height = 80, 40

	if cfg := m.composeConfig(); cfg != nil {
		t.Fatalf("config before presets load = %v, want nil (host rejects, no silent default)", cfg)
	}

	// The host serves backend-scoped lists (GET /presets?backend=).
	claude := make([]presets.Preset, 0, 2)
	for _, p := range presets.Sandbox() {
		if p.Backend == agent.BackendClaudeCode {
			claude = append(claude, p)
		}
	}
	model, _ := m.Update(presetsResultMsg{backend: agent.BackendClaudeCode, presets: claude})
	m = model.(*SessionViewModel)

	cfg := m.composeConfig()
	if cfg["model"] != "default" || cfg["effort"] != "default" || cfg[agent.ConfigOptionMode] != "bypassPermissions" {
		t.Fatalf("config = %v, want the Default preset verbatim", cfg)
	}

	// Tab to the Plan preset: the config is THAT preset, wholesale — not
	// a mode knob twiddled on top of Default.
	model, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = model.(*SessionViewModel)
	cfg = m.composeConfig()
	if cfg[agent.ConfigOptionMode] != "plan" || cfg["model"] != "default" || cfg["effort"] != "default" {
		t.Fatalf("config after Tab = %v, want the Plan preset", cfg)
	}

	// A model pick (picker confirm) overlays the preset's model with the
	// agent-advertised value id.
	m.models = []agent.ModelInfo{{ID: "opus", Name: "Opus"}}
	m.selectedModel = 0
	cfg = m.composeConfig()
	if cfg["model"] != "opus" || cfg[agent.ConfigOptionMode] != "plan" {
		t.Fatalf("config after model pick = %v, want model overlaid on the preset", cfg)
	}

	// Presets for another backend must not satisfy this one.
	m2 := newSessionViewComposingWithBackend(nil, "/tmp/project", agent.BackendCodex)
	model, _ = m2.Update(presetsResultMsg{backend: agent.BackendClaudeCode, presets: presets.Sandbox()})
	m2 = model.(*SessionViewModel)
	if cfg := m2.composeConfig(); cfg != nil {
		t.Fatalf("cross-backend presets satisfied codex compose: %v", cfg)
	}
}

// shift+tab opens the model picker in its loading state and kicks the
// on-demand config-options probe — the only spinner moment. esc cancels
// the wait; a result for a stale backend is dropped.
func TestCompose_ModelPickerProbesOnOpen(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "", agent.BackendClaudeCode)
	m.width, m.height = 80, 40

	model, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	m = model.(*SessionViewModel)
	if !m.showModelPicker || !m.modelOptionsLoading {
		t.Fatalf("shift+tab: showModelPicker=%v loading=%v, want both true", m.showModelPicker, m.modelOptionsLoading)
	}
	if !batchYields[configOptionsResultMsg](t, cmd) {
		t.Fatal("shift+tab dispatched no config-options probe")
	}

	// esc cancels the wait.
	model, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = model.(*SessionViewModel)
	if m.showModelPicker || m.modelOptionsLoading {
		t.Fatalf("esc during load: showModelPicker=%v loading=%v, want both false", m.showModelPicker, m.modelOptionsLoading)
	}
}

// The probe result builds the picker from the model option's values —
// value ids as IDs (what the config channel sends), group labels as the
// provider column. An errored or optionless probe closes the picker with
// a visible reason instead of rendering an empty list.
func TestCompose_ConfigOptionsResultBuildsModelPicker(t *testing.T) {
	t.Parallel()
	m := newSessionViewComposingWithBackend(nil, "", agent.BackendOpenCode)
	m.width, m.height = 80, 40
	m.showModelPicker = true
	m.modelOptionsLoading = true
	m.selectedModel = 0 // pre-set: skips the saved-preference lookup
	m.models = []agent.ModelInfo{{ID: "opencode/big-pickle"}}

	model, _ := m.Update(configOptionsResultMsg{
		backend: agent.BackendOpenCode,
		options: []agent.ConfigOption{{
			ID: "model", Name: "Model", Category: agent.ConfigOptionModel, CurrentValue: "opencode/big-pickle",
			Values: []agent.ConfigOptionValue{
				{Value: "opencode/big-pickle", Name: "Big Pickle", Group: "opencode"},
				{Value: "github-copilot/claude-sonnet-5", Name: "Claude Sonnet 5", Group: "github-copilot"},
			},
		}},
	})
	m = model.(*SessionViewModel)
	if m.modelOptionsLoading || !m.showModelPicker {
		t.Fatalf("after result: loading=%v picker=%v, want picker open and loaded", m.modelOptionsLoading, m.showModelPicker)
	}
	if len(m.models) != 2 || m.models[0].ID != "opencode/big-pickle" || m.models[1].ProviderID != "github-copilot" {
		t.Fatalf("picker models = %+v, want value ids + group-as-provider", m.models)
	}

	// An error closes the picker with a reason.
	m2 := newSessionViewComposingWithBackend(nil, "", agent.BackendOpenCode)
	m2.width, m2.height = 80, 40
	m2.showModelPicker = true
	m2.modelOptionsLoading = true
	model, _ = m2.Update(configOptionsResultMsg{backend: agent.BackendOpenCode, err: fmt.Errorf("adapter down")})
	m2 = model.(*SessionViewModel)
	if m2.showModelPicker || m2.err == nil {
		t.Fatalf("errored probe: picker=%v err=%v, want closed with a reason", m2.showModelPicker, m2.err)
	}

	// A stale result (backend toggled mid-probe) is dropped.
	m3 := newSessionViewComposingWithBackend(nil, "", agent.BackendCodex)
	m3.width, m3.height = 80, 40
	m3.showModelPicker = true
	m3.modelOptionsLoading = true
	model, _ = m3.Update(configOptionsResultMsg{backend: agent.BackendOpenCode, options: nil})
	m3 = model.(*SessionViewModel)
	if len(m3.models) != 0 {
		t.Fatalf("stale probe populated models: %+v", m3.models)
	}
}

// launchSession must not submit while presets haven't loaded yet —
// composeConfig() is nil then, and the host would reject the create with
// 400 config_incomplete anyway. Gating here surfaces an immediate reason
// instead of a round-trip failure.
func TestLaunchSession_BlocksUntilPresetsLoad(t *testing.T) {
	t.Parallel()
	dir := initGitRepoForCompose(t)
	m := newSessionViewComposingWithBackend(nil, dir, agent.BackendClaudeCode)
	m.width, m.height = 80, 40
	m.input.SetValue("fix the bug")

	model, cmd := m.launchSession()
	m = model.(*SessionViewModel)
	if cmd != nil {
		t.Fatal("launchSession returned a submit command before presets loaded")
	}
	if m.submitting {
		t.Fatal("submitting=true before presets loaded")
	}
	if m.err == nil {
		t.Fatal("expected m.err explaining why submit was blocked")
	}

	model, _ = m.Update(presetsResultMsg{backend: agent.BackendClaudeCode, presets: presets.Sandbox()})
	m = model.(*SessionViewModel)
	_, cmd = m.launchSession()
	if cmd == nil {
		t.Fatal("expected launchSession to submit once presets loaded")
	}
}

// A follow-up send must omit config when the picker still matches the
// session's current mode — the spec treats omitted config as "no change,"
// so resending an unchanged mode on every message would violate that
// invariant (and reassert state the session already has).
func TestModeConfigForSend_OmitsWhenModeUnchanged(t *testing.T) {
	t.Parallel()
	sel := selectableMode{perm: agent.ClaudePermPlan}
	info := &agent.SessionInfo{CurrentModeID: "plan"}
	if cfg := modeConfigForSend(sel, info); cfg != nil {
		t.Fatalf("config = %v, want nil (picker matches session's current mode)", cfg)
	}
}

func TestModeConfigForSend_IncludesWhenModeChanged(t *testing.T) {
	t.Parallel()
	sel := selectableMode{perm: agent.ClaudePermPlan}
	info := &agent.SessionInfo{CurrentModeID: "bypassPermissions"}
	cfg := modeConfigForSend(sel, info)
	if cfg[agent.ConfigOptionMode] != "plan" {
		t.Fatalf("config = %v, want mode=plan", cfg)
	}
}

// Before session info has ever loaded, there is no "current mode" to
// compare against — an explicit picker value must still ride along.
func TestModeConfigForSend_NilInfoSendsExplicitPick(t *testing.T) {
	t.Parallel()
	sel := selectableMode{perm: agent.ClaudePermPlan}
	if cfg := modeConfigForSend(sel, nil); cfg[agent.ConfigOptionMode] != "plan" {
		t.Fatalf("config = %v, want mode=plan when session info is unknown", cfg)
	}
}
