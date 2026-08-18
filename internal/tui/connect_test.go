package tui

// ConnectModel state-machine tests. Caller is nil throughout: messages
// are fed directly and the returned cmds asserted, mirroring the
// providerauth_test.go pattern — nothing here runs a cmd, so the caller
// is never dialed.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
)

// keyPress builds the KeyPressMsg for a single named key.
func keyPress(name string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: keyCodeFor(name), Text: textFor(name)})
}

func keyCodeFor(name string) rune {
	switch name {
	case "enter":
		return tea.KeyEnter
	case "down":
		return tea.KeyDown
	case "up":
		return tea.KeyUp
	case "esc":
		return tea.KeyEscape
	default:
		return []rune(name)[0]
	}
}

func textFor(name string) string {
	switch name {
	case "enter", "down", "up", "esc":
		return ""
	default:
		return name
	}
}

// isQuitCmd runs cmd and reports whether it resolves to tea.Quit.
func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// The picker must list every backend clank can launch — including ones
// with no provider in the catalog. "clank can run this, you just haven't
// connected it" is the whole point of the screen, so a backend that is
// missing from /auth/providers must still get a row.
func TestConnectBackendRows_ListsEveryBackend(t *testing.T) {
	t.Parallel()
	rows := connectBackendRows([]agent.ProviderAuthInfo{
		{ProviderID: "github-copilot", DisplayName: "GitHub Copilot", Backend: agent.BackendOpenCode, Connected: true},
		{ProviderID: "anthropic-api", Backend: agent.BackendClaudeCode},
	}, map[agent.BackendType]bool{agent.BackendOpenCode: true})
	if len(rows) != len(agent.AllBackends) {
		t.Fatalf("got %d rows, want one per backend (%d)", len(rows), len(agent.AllBackends))
	}
	state := make(map[agent.BackendType]connectBackendRow, len(rows))
	for i, row := range rows {
		if row.Backend != agent.AllBackends[i] {
			t.Errorf("row %d = %s, want %s (display order must follow AllBackends)", i, row.Backend, agent.AllBackends[i])
		}
		state[row.Backend] = row
	}
	if !state[agent.BackendOpenCode].IsAllowed || state[agent.BackendOpenCode].ProviderName != "GitHub Copilot" {
		t.Errorf("opencode row = %+v, want allowed with GitHub Copilot", state[agent.BackendOpenCode])
	}
	if state[agent.BackendClaudeCode].IsAllowed {
		t.Error("claude-code is not allowed")
	}
	if state[agent.BackendCodex].IsAllowed {
		t.Error("codex is absent from the allow map and must not read as allowed")
	}
}

// A named, disallowed backend asks for control permission before any provider
// catalog is loaded. This is the privacy boundary of `clank connect claude`.
func TestConnectModel_NamedBackendAsksAllowBeforeDetection(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, agent.BackendClaudeCode)
	model, _ := m.Update(connectProvidersLoadedMsg{})
	m = model.(*ConnectModel)
	if m.phase != connectPhaseAllow {
		t.Fatalf("phase = %v, want allow phase", m.phase)
	}
	if got := m.Result().Backend; got != agent.BackendClaudeCode {
		t.Errorf("Result().Backend = %q, want %q before the flow even starts", got, agent.BackendClaudeCode)
	}
	if !strings.Contains(m.body(), "Allow Clank to control Claude Code?") || !strings.Contains(m.body(), "Agent Client Protocol") {
		t.Errorf("allow prompt is missing its control boundary:\n%s", m.body())
	}
}

// Picking a row hands off to the provider flow scoped to that backend,
// and records the choice immediately — a run canceled mid-auth still
// reports which backend the user was connecting.
func TestConnectModel_PickingDisallowedBackendAsksAllow(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, "")
	model, _ := m.Update(connectProvidersLoadedMsg{providers: []agent.ProviderAuthInfo{
		{ProviderID: "github-copilot", Backend: agent.BackendOpenCode},
	}})
	m = model.(*ConnectModel)
	if m.phase != connectPhasePickBackend {
		t.Fatalf("phase = %v, want the backend picker", m.phase)
	}

	// Move to the second row (claude-code in AllBackends order) and pick it.
	model, _ = m.Update(keyPress("down"))
	model, cmd := model.(*ConnectModel).Update(keyPress("enter"))
	m = model.(*ConnectModel)

	if m.phase != connectPhaseAllow {
		t.Fatalf("phase = %v, want allow phase after enter", m.phase)
	}
	want := agent.AllBackends[1]
	if m.Result().Backend != want {
		t.Errorf("Result().Backend = %q, want %q", m.Result().Backend, want)
	}
	if m.Result().IsConnected {
		t.Error("picking a backend is not connecting it")
	}
	if cmd != nil {
		t.Error("opening the allow prompt must not detect providers yet")
	}
}

func TestConnectModel_AllowThenDetectedAuthReturnsToPicker(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, "")
	model, _ := m.Update(connectProvidersLoadedMsg{})
	model, _ = model.(*ConnectModel).Update(keyPress("enter"))
	m = model.(*ConnectModel)
	model, _ = m.Update(connectHarnessAllowedMsg{
		backend: agent.BackendOpenCode,
		providers: []agent.ProviderAuthInfo{{
			Backend: agent.BackendOpenCode, DisplayName: "GitHub Copilot", Connected: true,
		}},
	})
	m = model.(*ConnectModel)
	if m.phase != connectPhasePickBackend {
		t.Fatalf("phase = %v, want picker after smooth detection", m.phase)
	}
	if got := m.backendRow(agent.BackendOpenCode); !got.IsAllowed || got.ProviderName != "GitHub Copilot" {
		t.Errorf("refreshed row = %+v", got)
	}
}

// The connect program is the root here, so the flow's terminal messages
// have to end it — the inbox used to be the thing that dismissed the
// modal.
func TestConnectModel_FlowTerminalMessagesQuit(t *testing.T) {
	t.Parallel()
	t.Run("done reports the connection", func(t *testing.T) {
		t.Parallel()
		m := reachedFlowSuccess(agent.BackendOpenCode)
		model, cmd := m.Update(providerAuthDoneMsg{})
		if !isQuitCmd(cmd) {
			t.Error("providerAuthDoneMsg must quit the standalone program")
		}
		if got := model.(*ConnectModel).Result(); !got.IsConnected || got.Backend != agent.BackendOpenCode {
			t.Errorf("Result() = %+v, want opencode connected", got)
		}
	})

	t.Run("cancel reports nothing connected", func(t *testing.T) {
		t.Parallel()
		m := NewConnectModel(nil, agent.BackendOpenCode)
		model, cmd := m.Update(providerAuthCancelMsg{})
		if !isQuitCmd(cmd) {
			t.Error("providerAuthCancelMsg must quit the standalone program")
		}
		if model.(*ConnectModel).Result().IsConnected {
			t.Error("a canceled flow must not report a connection")
		}
	})
}

// The credential is written before the success screen is drawn, so
// quitting at that screen — rather than pressing a key to dismiss it —
// must still report the connection. Reporting "nothing connected" there
// would send the user back through a flow they already completed.
func TestConnectModel_QuitAtSuccessStillReportsConnected(t *testing.T) {
	t.Parallel()
	m := reachedFlowSuccess(agent.BackendClaudeCode)
	model, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	if !isQuitCmd(cmd) {
		t.Fatal("ctrl+c at the success screen must quit")
	}
	if got := model.(*ConnectModel).Result(); !got.IsConnected {
		t.Errorf("Result() = %+v, want the completed connection reported", got)
	}
}

// reachedFlowSuccess returns a model whose hosted auth flow has reached
// its success phase — the state after a credential has been stored.
func reachedFlowSuccess(backend agent.BackendType) *ConnectModel {
	m := NewConnectModel(nil, backend)
	m.phase = connectPhaseProvider
	m.providerAuth.phase = providerPhaseSuccess
	m.providerAuth.activeProvider = agent.ProviderAuthInfo{DisplayName: "Test Provider"}
	return m
}

// providerAuthModel only handles esc; ctrl+c belongs to whatever hosts
// it. Without this the CLI would be unquittable mid-device-flow.
func TestConnectModel_CtrlCQuitsFromEveryPhase(t *testing.T) {
	t.Parallel()
	phases := map[string]*ConnectModel{
		"loading":  NewConnectModel(nil, ""),
		"provider": reachedFlowSuccess(agent.BackendOpenCode),
	}
	picker := NewConnectModel(nil, "")
	pickerModel, _ := picker.Update(connectProvidersLoadedMsg{})
	phases["picker"] = pickerModel.(*ConnectModel)

	for name, m := range phases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
			if !isQuitCmd(cmd) {
				t.Errorf("ctrl+c in the %s phase did not quit", name)
			}
		})
	}
}

// A catalog read that fails must surface the reason instead of showing
// an empty picker that looks like "no agents exist".
func TestConnectModel_CatalogErrorIsShown(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, "")
	model, _ := m.Update(connectProvidersLoadedMsg{err: errCatalogUnreachable})
	m = model.(*ConnectModel)
	if m.phase != connectPhaseError {
		t.Fatalf("phase = %v, want error phase", m.phase)
	}
	if !strings.Contains(m.body(), errCatalogUnreachable.Error()) {
		t.Errorf("view does not surface the failure:\n%s", m.body())
	}
}

func TestConnectModel_PickerViewShowsAllowAndAuthState(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, "")
	model, _ := m.Update(connectProvidersLoadedMsg{providers: []agent.ProviderAuthInfo{
		{ProviderID: "github-copilot", DisplayName: "GitHub Copilot", Backend: agent.BackendOpenCode, Connected: true},
	}, allowed: map[agent.BackendType]bool{agent.BackendOpenCode: true}})
	body := model.(*ConnectModel).body()
	if !strings.Contains(body, "GitHub Copilot") {
		t.Errorf("picker view shows no configured auth:\n%s", body)
	}
	if !strings.Contains(body, "allow") {
		t.Errorf("picker view shows no allow action:\n%s", body)
	}
	for _, bt := range agent.AllBackends {
		if !strings.Contains(body, backendDisplayName(bt)) {
			t.Errorf("picker view omits %s:\n%s", bt, body)
		}
	}
}

// The connect UI renders inline: `clank preview` prints around it, and
// an alt-screen would wipe that context on exit.
func TestConnectModel_RendersInline(t *testing.T) {
	t.Parallel()
	v := NewConnectModel(nil, "").View()
	if v.AltScreen {
		t.Error("connect must not take over the alt screen")
	}
}

var errCatalogUnreachable = errTestString("dial unix /clank.sock: connect: connection refused")

type errTestString string

func (e errTestString) Error() string { return string(e) }

// Leaving the provider list must return to the backend picker, not end
// the program: picking the wrong agent is a two-keystroke mistake and
// should cost two keystrokes to undo.
func TestConnectModel_LeavingProviderListReturnsToPicker(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, "")
	model, _ := m.Update(connectProvidersLoadedMsg{providers: []agent.ProviderAuthInfo{
		{ProviderID: "github-copilot", Backend: agent.BackendOpenCode},
	}, allowed: map[agent.BackendType]bool{agent.BackendOpenCode: true}})
	model, _ = model.(*ConnectModel).Update(keyPress("enter"))
	if model.(*ConnectModel).phase != connectPhaseProvider {
		t.Fatal("setup: enter did not open the provider flow")
	}

	model, cmd := model.(*ConnectModel).Update(providerAuthCancelMsg{})
	m = model.(*ConnectModel)
	if isQuitCmd(cmd) {
		t.Fatal("backing out of the provider list must not quit")
	}
	if m.phase != connectPhasePickBackend {
		t.Fatalf("phase = %v, want the backend picker", m.phase)
	}
	// The picker still works: its rows survived the round trip.
	if len(m.backends) != len(agent.AllBackends) {
		t.Errorf("picker came back with %d rows, want %d", len(m.backends), len(agent.AllBackends))
	}
}

// `clank connect claude` has no picker behind it, so backing out of the
// provider list is the end of the program — not a jump to a screen the
// user never asked for.
func TestConnectModel_NamedBackendHasNothingToGoBackTo(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, agent.BackendClaudeCode)
	m.phase = connectPhaseProvider
	_, cmd := m.Update(providerAuthCancelMsg{})
	if !isQuitCmd(cmd) {
		t.Error("a named-backend run must quit when the provider list is left")
	}
}

// q quits from anywhere — the picker, the provider list, a device-flow
// wait — so there is always a one-key exit that isn't ctrl+c.
func TestConnectModel_QQuitsFromEveryNonTextPhase(t *testing.T) {
	t.Parallel()
	loading := NewConnectModel(nil, "")

	pickerModel, _ := NewConnectModel(nil, "").Update(connectProvidersLoadedMsg{})
	picker := pickerModel.(*ConnectModel)

	providerList := NewConnectModel(nil, agent.BackendOpenCode)
	providerList.phase = connectPhaseProvider
	providerList.providerAuth.phase = providerPhaseList

	confirm := NewConnectModel(nil, agent.BackendOpenCode)
	confirm.phase = connectPhaseProvider
	confirm.providerAuth.phase = providerPhaseConfirm

	awaiting := NewConnectModel(nil, agent.BackendOpenCode)
	awaiting.phase = connectPhaseProvider
	awaiting.providerAuth.phase = providerPhaseAwaiting

	for name, m := range map[string]*ConnectModel{
		"loading":        loading,
		"backend picker": picker,
		"provider list":  providerList,
		"confirm gate":   confirm,
		"awaiting":       awaiting,
		"success":        reachedFlowSuccess(agent.BackendOpenCode),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, cmd := m.Update(keyPress("q")); !isQuitCmd(cmd) {
				t.Errorf("q in the %s phase did not quit", name)
			}
		})
	}
}

// …except while a field is focused, where q is a character. An API key
// or a pasted auth code containing "q" must type, not quit.
func TestConnectModel_QIsTypedIntoFocusedFields(t *testing.T) {
	t.Parallel()
	for _, phase := range []providerAuthPhase{providerPhaseAPIKey, providerPhaseOAuthCode} {
		m := NewConnectModel(nil, agent.BackendOpenCode)
		m.phase = connectPhaseProvider
		m.providerAuth = newProviderAuthModel(nil, agent.BackendOpenCode, "")
		m.providerAuth.phase = phase
		m.providerAuth.apiKey.Focus()

		model, cmd := m.Update(keyPress("q"))
		if isQuitCmd(cmd) {
			t.Fatalf("q in phase %v quit instead of typing", phase)
		}
		if got := model.(*ConnectModel).providerAuth.apiKey.Value(); got != "q" {
			t.Errorf("phase %v: field holds %q, want the typed \"q\"", phase, got)
		}
	}
}

// Quitting mid-flow must abort it on the host first: this process
// exiting does not stop a device poll or a setup-token PTY that
// clank-host started, so a bare tea.Quit would leak one.
func TestConnectModel_QuittingMidFlowCancelsItOnTheHost(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, agent.BackendClaudeCode)
	m.phase = connectPhaseProvider
	m.providerAuth.phase = providerPhaseAwaiting
	m.providerAuth.flow = agent.DeviceFlowStart{FlowID: "flow-1"}

	// A live flow must not resolve straight to Quit — the cancel has to
	// run first. (Sequence resolves to its own internal message, so the
	// assertion is that this is a command, and not a bare quit.)
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	if cmd == nil {
		t.Fatal("ctrl+c mid-flow produced no command at all")
	}
	if isQuitCmd(cmd) {
		t.Error("quitting mid-flow skipped the host-side cancel")
	}

	// A settled flow has nothing to cancel, so it quits directly — and
	// must not "cancel" a connection that already succeeded.
	settled := reachedFlowSuccess(agent.BackendClaudeCode)
	settled.providerAuth.flow = agent.DeviceFlowStart{FlowID: "flow-1"}
	model, cmd := settled.Update(keyPress("q"))
	if !isQuitCmd(cmd) {
		t.Error("a settled flow must quit directly")
	}
	if settled.providerAuth.hasLiveFlow() {
		t.Error("a succeeded flow must not read as still running on the host")
	}
	if !model.(*ConnectModel).Result().IsConnected {
		t.Error("quitting after success must still report the connection")
	}
}
