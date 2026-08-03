package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
)

// awaitingLabel must NOT mention "OpenCode server" for non-opencode
// backends — they have no OpenCode restart. Regression for a v1 ship
// where the authorized-phase copy was hardcoded for opencode and
// surfaced "restarting OpenCode" while connecting a Claude
// subscription token (and later would have done the same for codex).
func TestAwaitingLabel_NonOpenCodeDoesNotMentionOpenCode(t *testing.T) {
	t.Parallel()
	for _, backend := range []agent.BackendType{agent.BackendClaudeCode, agent.BackendCodex} {
		for _, state := range []agent.DeviceFlowState{
			agent.DeviceFlowPending,
			agent.DeviceFlowAuthorized,
		} {
			for _, at := range []string{agent.AuthTypeAPI, agent.AuthTypeOAuthCode, agent.AuthTypeDevice} {
				label := awaitingLabel(state, agent.ProviderAuthInfo{AuthType: at, Backend: backend})
				if strings.Contains(strings.ToLower(label), "opencode") {
					t.Errorf("%s awaitingLabel(state=%v, type=%s) mentions opencode: %q", backend, state, at, label)
				}
			}
		}
	}
}

// OpenCode authorized-phase copy must still surface the restart
// expectation so users understand the 10–15s wait; codex's authorized
// copy names its adapter instead.
func TestAwaitingLabel_AuthorizedMentionsRestart(t *testing.T) {
	t.Parallel()
	label := awaitingLabel(agent.DeviceFlowAuthorized, agent.ProviderAuthInfo{AuthType: agent.AuthTypeAPI, Backend: agent.BackendOpenCode})
	if !strings.Contains(strings.ToLower(label), "restart") {
		t.Errorf("opencode authorized label should mention restart, got %q", label)
	}
	label = awaitingLabel(agent.DeviceFlowAuthorized, agent.ProviderAuthInfo{AuthType: agent.AuthTypeDevice, Backend: agent.BackendCodex})
	if !strings.Contains(strings.ToLower(label), "codex") {
		t.Errorf("codex authorized label should mention the codex adapter, got %q", label)
	}
}

// oauth-code's pending-phase label must mention exchanging/verifying
// so the user understands what's happening between code-paste and
// success. Pins the v2 PTY-relay UX.
func TestAwaitingLabel_OAuthCodePendingMentionsExchange(t *testing.T) {
	t.Parallel()
	label := awaitingLabel(agent.DeviceFlowPending, agent.ProviderAuthInfo{AuthType: agent.AuthTypeOAuthCode, Backend: agent.BackendClaudeCode})
	low := strings.ToLower(label)
	if !strings.Contains(low, "exchang") && !strings.Contains(low, "verif") {
		t.Errorf("oauth-code pending label should describe code exchange, got %q", label)
	}
}

// The oauth-code phase must begin polling the moment it's entered and
// keep polling on each tick, so a native-local flow that self-completes
// via setup-token's own browser callback is detected without the user
// ever pasting a code. Regression for the deadlock where the code phase
// blocked on a paste and never polled. Caller is nil on purpose: we feed
// the messages directly and assert the returned cmds, never running them.
func TestProviderAuth_OAuthCodePhasePolls(t *testing.T) {
	t.Parallel()
	m := newProviderAuthModel(nil, agent.BackendClaudeCode, "")
	m.activeProvider = agent.ProviderAuthInfo{
		ProviderID: host.ProviderAnthropicClaudeCode,
		AuthType:   agent.AuthTypeOAuthCode,
	}

	// Flow started: the host returned the authorize URL. We must land in
	// the code phase AND kick off polling (non-nil cmd batch).
	m, cmd := m.Update(providerStartedMsg{start: agent.DeviceFlowStart{
		FlowID:          "flow-1",
		VerificationURL: "https://claude.com/cai/oauth/authorize?x=1",
	}})
	if m.phase != providerPhaseOAuthCode {
		t.Fatalf("phase=%v, want OAuthCode", m.phase)
	}
	if cmd == nil {
		t.Fatal("entering the code phase must start polling (got nil cmd)")
	}

	// A tick during the code phase must keep polling.
	if _, tickCmd := m.Update(providerPollTickMsg{}); tickCmd == nil {
		t.Fatal("poll tick during OAuthCode phase must continue polling (got nil cmd)")
	}
}

// A success status arriving while still in the code phase — i.e. the
// local flow self-completed with no pasted code — must transition
// straight to success.
func TestProviderAuth_OAuthCodeSelfCompletesToSuccess(t *testing.T) {
	t.Parallel()
	m := newProviderAuthModel(nil, agent.BackendClaudeCode, "")
	m.activeProvider = agent.ProviderAuthInfo{
		ProviderID: host.ProviderAnthropicClaudeCode,
		AuthType:   agent.AuthTypeOAuthCode,
	}
	m.phase = providerPhaseOAuthCode

	m, _ = m.Update(providerStatusMsg{status: agent.DeviceFlowStatus{State: agent.DeviceFlowSuccess}})
	if m.phase != providerPhaseSuccess {
		t.Fatalf("phase=%v, want Success (self-complete must not require a paste)", m.phase)
	}
}

// providerSectionBreakpoints feeds the shared nextBreakpoint /
// prevBreakpoint helpers used by shift+up / shift+down in the provider
// list view. Index 0 is always the first breakpoint; later breakpoints
// are the positions where the Backend field changes from the entry
// above.
func TestProviderSectionBreakpoints(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		providers []agent.ProviderAuthInfo
		want      []int
	}{
		{
			name:      "empty list yields no breakpoints",
			providers: nil,
			want:      nil,
		},
		{
			name: "single backend yields a single top-of-list breakpoint",
			providers: []agent.ProviderAuthInfo{
				{ProviderID: "a", Backend: agent.BackendClaudeCode},
				{ProviderID: "b", Backend: agent.BackendClaudeCode},
			},
			want: []int{0},
		},
		{
			name: "Claude Code then OpenCode catalog (current order)",
			providers: []agent.ProviderAuthInfo{
				{ProviderID: "anthropic-claude-code", Backend: agent.BackendClaudeCode},
				{ProviderID: "anthropic-api", Backend: agent.BackendClaudeCode},
				{ProviderID: "github-copilot", Backend: agent.BackendOpenCode},
				{ProviderID: "openai", Backend: agent.BackendOpenCode},
				{ProviderID: "google", Backend: agent.BackendOpenCode},
			},
			want: []int{0, 2},
		},
		{
			name: "alternating backends yields a breakpoint per transition",
			providers: []agent.ProviderAuthInfo{
				{ProviderID: "a", Backend: agent.BackendClaudeCode},
				{ProviderID: "b", Backend: agent.BackendOpenCode},
				{ProviderID: "c", Backend: agent.BackendClaudeCode},
			},
			want: []int{0, 1, 2},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := providerSectionBreakpoints(c.providers)
			if !equalIntSlice(got, c.want) {
				t.Errorf("providerSectionBreakpoints(%d entries) = %v, want %v", len(c.providers), got, c.want)
			}
		})
	}
}

// shift+down should advance the cursor from the top of Claude Code to
// the top of OpenCode in the current catalog ordering; subsequent
// shift+downs clamp at the last breakpoint. shift+up reverses.
// Together this pins the "jump to next backend" UX so future catalog
// reshuffles or grouping tweaks can't silently regress it.
func TestProviderSectionBreakpoints_NavigationFlow(t *testing.T) {
	t.Parallel()
	providers := []agent.ProviderAuthInfo{
		{ProviderID: "anthropic-claude-code", Backend: agent.BackendClaudeCode},
		{ProviderID: "anthropic-api", Backend: agent.BackendClaudeCode},
		{ProviderID: "github-copilot", Backend: agent.BackendOpenCode},
		{ProviderID: "openai", Backend: agent.BackendOpenCode},
		{ProviderID: "google", Backend: agent.BackendOpenCode},
	}
	bp := providerSectionBreakpoints(providers)

	// shift+down from top of Claude Code (0) → top of OpenCode (2).
	if got := nextBreakpoint(bp, 0); got != 2 {
		t.Errorf("nextBreakpoint(bp, 0) = %d, want 2", got)
	}
	// shift+down from mid-Claude (1) → top of OpenCode (2).
	if got := nextBreakpoint(bp, 1); got != 2 {
		t.Errorf("nextBreakpoint(bp, 1) = %d, want 2", got)
	}
	// shift+down at top of last section (2) clamps at 2.
	if got := nextBreakpoint(bp, 2); got != 2 {
		t.Errorf("nextBreakpoint(bp, 2) = %d, want 2 (clamp)", got)
	}
	// shift+up from top of OpenCode (2) → top of Claude Code (0).
	if got := prevBreakpoint(bp, 2); got != 0 {
		t.Errorf("prevBreakpoint(bp, 2) = %d, want 0", got)
	}
	// shift+up from mid-OpenCode (3) → top of OpenCode (2).
	if got := prevBreakpoint(bp, 3); got != 2 {
		t.Errorf("prevBreakpoint(bp, 3) = %d, want 2", got)
	}
	// shift+up at top of first section (0) clamps at 0.
	if got := prevBreakpoint(bp, 0); got != 0 {
		t.Errorf("prevBreakpoint(bp, 0) = %d, want 0 (clamp)", got)
	}
}

func equalIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// esc means "back one screen", not "throw the flow away". The confirm
// gate offers n to return to the list; esc used to dismiss the whole
// modal instead, so a mis-picked provider cost you the entire flow.
func TestProviderAuth_EscFromConfirmReturnsToList(t *testing.T) {
	t.Parallel()
	m := newProviderAuthModel(nil, agent.BackendOpenCode, "")
	m.providers = []agent.ProviderAuthInfo{{ProviderID: "openai", Backend: agent.BackendOpenCode}}
	m.phase = providerPhaseConfirm
	m.activeProvider = m.providers[0]

	m, cmd := m.Update(keyPress("esc"))
	if m.phase != providerPhaseList {
		t.Fatalf("phase = %v, want the provider list", m.phase)
	}
	if isCancelCmd(cmd) {
		t.Error("esc at the confirm gate must not dismiss the modal")
	}
}

// One screen back from the key form is the confirm gate — and the
// half-filled form's error state must not follow you there.
func TestProviderAuth_EscFromAPIKeyReturnsToConfirm(t *testing.T) {
	t.Parallel()
	m := newProviderAuthModel(nil, agent.BackendOpenCode, "")
	m.phase = providerPhaseAPIKey
	m.errMsg = "value cannot be empty"

	m, cmd := m.Update(keyPress("esc"))
	if m.phase != providerPhaseConfirm {
		t.Fatalf("phase = %v, want the confirm gate", m.phase)
	}
	if m.errMsg != "" {
		t.Errorf("stale error %q carried back", m.errMsg)
	}
	if isCancelCmd(cmd) {
		t.Error("esc in the key form must not dismiss the modal")
	}
}

// Backing out of a phase with a flow running on the host must abort it
// — a `claude setup-token` PTY or a device poll would otherwise outlive
// the screen that started it — and then land back on the list.
func TestProviderAuth_EscFromLiveFlowCancelsItAndReturnsToList(t *testing.T) {
	t.Parallel()
	for _, phase := range []providerAuthPhase{providerPhaseAwaiting, providerPhaseOAuthCode} {
		m := newProviderAuthModel(nil, agent.BackendClaudeCode, "")
		m.providers = []agent.ProviderAuthInfo{{ProviderID: host.ProviderAnthropicClaudeCode}}
		m.phase = phase
		m.activeProvider = m.providers[0]

		// No flow id: cancelFlowCmd short-circuits the host call, so the
		// nil caller is never dialed and the message is what we assert.
		_, cmd := m.Update(keyPress("esc"))
		if cmd == nil {
			t.Fatalf("phase %v: esc produced no command", phase)
		}
		if _, ok := cmd().(providerFlowCanceledMsg); !ok {
			t.Fatalf("phase %v: esc did not cancel the flow and step back", phase)
		}
	}

	// And the message itself returns to the list with the flow cleared.
	m := newProviderAuthModel(nil, agent.BackendClaudeCode, "")
	m.phase = providerPhaseAwaiting
	m.flow = agent.DeviceFlowStart{FlowID: "flow-1", UserCode: "ABCD"}
	m, _ = m.Update(providerFlowCanceledMsg{})
	if m.phase != providerPhaseList {
		t.Errorf("phase = %v, want the provider list", m.phase)
	}
	if m.flow.FlowID != "" {
		t.Errorf("canceled flow %q still attached", m.flow.FlowID)
	}
}

// A failure is when you most want to retry, so the error screen returns
// to the list — unless the failure is what left the list empty (the
// initial catalog load), where there is nothing to return to.
func TestProviderAuth_ErrorScreenReturnsToListWhenThereIsOne(t *testing.T) {
	t.Parallel()
	m := newProviderAuthModel(nil, agent.BackendOpenCode, "")
	m.providers = []agent.ProviderAuthInfo{{ProviderID: "openai", Backend: agent.BackendOpenCode}}
	m.phase = providerPhaseError
	m.errMsg = "invalid api key"

	m, cmd := m.Update(keyPress("esc"))
	if m.phase != providerPhaseList {
		t.Fatalf("phase = %v, want the provider list", m.phase)
	}
	if m.errMsg != "" {
		t.Errorf("stale error %q carried back", m.errMsg)
	}
	if isCancelCmd(cmd) {
		t.Error("a retryable error must not dismiss the modal")
	}

	// Catalog load failed: no list exists, so dismiss.
	empty := newProviderAuthModel(nil, agent.BackendOpenCode, "")
	empty.phase = providerPhaseError
	empty.errMsg = "connection refused"
	_, cmd = empty.Update(keyPress("esc"))
	if !isCancelCmd(cmd) {
		t.Error("an error with no list behind it must dismiss the modal")
	}
}

// isCancelCmd runs cmd and reports whether it dismisses the modal.
func isCancelCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(providerAuthCancelMsg)
	return ok
}

// hasLiveFlow decides whether backing out has to call the host. It must
// be true exactly while a flow is running there — a completed one would
// otherwise be "canceled" on the way out, and a phase with no flow at
// all would produce a pointless call.
func TestProviderAuth_HasLiveFlow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		phase  providerAuthPhase
		flowID string
		want   bool
	}{
		{name: "awaiting a device authorization", phase: providerPhaseAwaiting, flowID: "f1", want: true},
		{name: "waiting on a pasted auth code", phase: providerPhaseOAuthCode, flowID: "f1", want: true},
		{name: "awaiting before a flow id exists", phase: providerPhaseAwaiting, want: false},
		{name: "browsing the list", phase: providerPhaseList, flowID: "f1", want: false},
		{name: "filling the key form", phase: providerPhaseAPIKey, flowID: "f1", want: false},
		{name: "already succeeded", phase: providerPhaseSuccess, flowID: "f1", want: false},
		{name: "already failed", phase: providerPhaseError, flowID: "f1", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := newProviderAuthModel(nil, agent.BackendClaudeCode, "")
			m.phase = c.phase
			m.flow = agent.DeviceFlowStart{FlowID: c.flowID}
			if got := m.hasLiveFlow(); got != c.want {
				t.Errorf("hasLiveFlow() = %v, want %v", got, c.want)
			}
		})
	}
}

// The screens that own a focused field must be the only ones that treat
// a bare letter as content — that is what stops a host's "q quits" from
// eating a keystroke meant for an API key.
func TestProviderAuth_AcceptsTextInput(t *testing.T) {
	t.Parallel()
	typing := map[providerAuthPhase]bool{
		providerPhaseAPIKey:    true,
		providerPhaseOAuthCode: true,
	}
	for _, phase := range []providerAuthPhase{
		providerPhaseLoading, providerPhaseList, providerPhaseConfirm,
		providerPhaseAPIKey, providerPhaseOAuthCode, providerPhaseAwaiting,
		providerPhaseSuccess, providerPhaseError,
	} {
		m := newProviderAuthModel(nil, agent.BackendOpenCode, "")
		m.phase = phase
		if got := m.acceptsTextInput(); got != typing[phase] {
			t.Errorf("phase %v: acceptsTextInput() = %v, want %v", phase, got, typing[phase])
		}
	}
}
