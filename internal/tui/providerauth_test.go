package tui

import (
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// awaitingLabel must NOT mention "OpenCode server" for Anthropic
// providers — they have no server restart. Regression for a v1 ship
// where the authorized-phase copy was hardcoded for opencode and
// surfaced "restarting OpenCode" while connecting a Claude
// subscription token.
func TestAwaitingLabel_AnthropicDoesNotMentionOpenCode(t *testing.T) {
	t.Parallel()
	for _, state := range []agent.DeviceFlowState{
		agent.DeviceFlowPending,
		agent.DeviceFlowAuthorized,
	} {
		for _, at := range []string{agent.AuthTypeAPI, agent.AuthTypeOAuthCode} {
			label := awaitingLabel(state, at, true /*isAnthropic*/)
			if strings.Contains(strings.ToLower(label), "opencode") {
				t.Errorf("anthropic awaitingLabel(state=%v, type=%s) mentions opencode: %q", state, at, label)
			}
		}
	}
}

// OpenCode authorized-phase copy must still surface the restart
// expectation so users understand the 10–15s wait.
func TestAwaitingLabel_OpenCodeAuthorizedMentionsRestart(t *testing.T) {
	t.Parallel()
	label := awaitingLabel(agent.DeviceFlowAuthorized, agent.AuthTypeAPI, false)
	if !strings.Contains(strings.ToLower(label), "restart") {
		t.Errorf("opencode authorized label should mention restart, got %q", label)
	}
}

// oauth-code's pending-phase label must mention exchanging/verifying
// so the user understands what's happening between code-paste and
// success. Pins the v2 PTY-relay UX.
func TestAwaitingLabel_OAuthCodePendingMentionsExchange(t *testing.T) {
	t.Parallel()
	label := awaitingLabel(agent.DeviceFlowPending, agent.AuthTypeOAuthCode, true)
	low := strings.ToLower(label)
	if !strings.Contains(low, "exchang") && !strings.Contains(low, "verif") {
		t.Errorf("oauth-code pending label should describe code exchange, got %q", label)
	}
}

func TestIsAnthropicProviderID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id   string
		want bool
	}{
		{host.ProviderAnthropicClaudeCode, true},
		{host.ProviderAnthropicAPI, true},
		{"openai", false},
		{"github-copilot", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isAnthropicProviderID(c.id); got != c.want {
			t.Errorf("isAnthropicProviderID(%q)=%v, want %v", c.id, got, c.want)
		}
	}
}
