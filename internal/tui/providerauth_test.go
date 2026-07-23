package tui

import (
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
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
