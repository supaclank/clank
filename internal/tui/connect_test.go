package tui

// ConnectModel state-machine tests. Caller is nil throughout: messages
// are fed directly and the returned cmds asserted, mirroring the
// providerauth_test.go pattern — nothing here runs a cmd, so the caller
// is never dialed.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/acksell/clank/internal/agent"
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
		{ProviderID: "github-copilot", Backend: agent.BackendOpenCode, Connected: true},
		{ProviderID: "anthropic-api", Backend: agent.BackendClaudeCode},
	})
	if len(rows) != len(agent.AllBackends) {
		t.Fatalf("got %d rows, want one per backend (%d)", len(rows), len(agent.AllBackends))
	}
	state := make(map[agent.BackendType]bool, len(rows))
	for i, row := range rows {
		if row.Backend != agent.AllBackends[i] {
			t.Errorf("row %d = %s, want %s (display order must follow AllBackends)", i, row.Backend, agent.AllBackends[i])
		}
		state[row.Backend] = row.IsConnected
	}
	if !state[agent.BackendOpenCode] {
		t.Error("opencode has a connected provider but its row says not connected")
	}
	if state[agent.BackendClaudeCode] {
		t.Error("claude-code has no connected provider but its row says connected")
	}
	if state[agent.BackendCodex] {
		t.Error("codex is absent from the catalog and must not read as connected")
	}
}

// Naming a backend skips the picker entirely — `clank connect claude`
// should land in Anthropic's provider list, not ask which agent you
// meant.
func TestConnectModel_NamedBackendSkipsPicker(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, agent.BackendClaudeCode)
	if m.phase != connectPhaseProvider {
		t.Fatalf("phase = %v, want provider phase", m.phase)
	}
	if got := m.Result().Backend; got != agent.BackendClaudeCode {
		t.Errorf("Result().Backend = %q, want %q before the flow even starts", got, agent.BackendClaudeCode)
	}
	if m.providerAuth.backend != agent.BackendClaudeCode {
		t.Errorf("hosted flow scoped to %q, want %q", m.providerAuth.backend, agent.BackendClaudeCode)
	}
}

// Picking a row hands off to the provider flow scoped to that backend,
// and records the choice immediately — a run canceled mid-auth still
// reports which backend the user was connecting.
func TestConnectModel_PickingBackendEntersScopedProviderFlow(t *testing.T) {
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

	if m.phase != connectPhaseProvider {
		t.Fatalf("phase = %v, want provider phase after enter", m.phase)
	}
	want := agent.AllBackends[1]
	if m.Result().Backend != want {
		t.Errorf("Result().Backend = %q, want %q", m.Result().Backend, want)
	}
	if m.providerAuth.backend != want {
		t.Errorf("hosted flow scoped to %q, want %q", m.providerAuth.backend, want)
	}
	if m.Result().IsConnected {
		t.Error("picking a backend is not connecting it")
	}
	if cmd == nil {
		t.Error("entering the provider phase must start loading its providers")
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
	m.providerAuth.phase = providerPhaseSuccess
	return m
}

// providerAuthModel only handles esc; ctrl+c belongs to whatever hosts
// it. Without this the CLI would be unquittable mid-device-flow.
func TestConnectModel_CtrlCQuitsFromEveryPhase(t *testing.T) {
	t.Parallel()
	phases := map[string]*ConnectModel{
		"loading":  NewConnectModel(nil, ""),
		"provider": NewConnectModel(nil, agent.BackendOpenCode),
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

// The picker must show connection state per row — it's the only reason
// to render the screen rather than defaulting silently.
func TestConnectModel_PickerViewShowsConnectionState(t *testing.T) {
	t.Parallel()
	m := NewConnectModel(nil, "")
	model, _ := m.Update(connectProvidersLoadedMsg{providers: []agent.ProviderAuthInfo{
		{ProviderID: "github-copilot", Backend: agent.BackendOpenCode, Connected: true},
	}})
	body := model.(*ConnectModel).body()
	if !strings.Contains(body, "connected") {
		t.Errorf("picker view shows no connection state:\n%s", body)
	}
	if !strings.Contains(body, "not connected") {
		t.Errorf("picker view never says a backend is unconnected:\n%s", body)
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
