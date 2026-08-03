package agent_test

import (
	"testing"

	"github.com/supaclank/clank/internal/agent"
)

// A borrowed credential (the machine's own claude CLI login, an env var)
// counts as connected — the spawned agent works, which is the only thing
// the first-run gate cares about. Reading Source instead of Connected
// here would nag users whose CLI is already signed in.
func TestIsAnyProviderConnected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		providers []agent.ProviderAuthInfo
		want      bool
	}{
		{
			name: "empty catalog",
			want: false,
		},
		{
			name: "catalog with nothing connected",
			providers: []agent.ProviderAuthInfo{
				{ProviderID: "github-copilot", Backend: agent.BackendOpenCode},
				{ProviderID: "anthropic-api", Backend: agent.BackendClaudeCode},
			},
			want: false,
		},
		{
			name: "stored credential",
			providers: []agent.ProviderAuthInfo{
				{ProviderID: "github-copilot", Backend: agent.BackendOpenCode},
				{ProviderID: "anthropic-api", Backend: agent.BackendClaudeCode, Connected: true},
			},
			want: true,
		},
		{
			name: "credential borrowed from the machine's claude CLI",
			providers: []agent.ProviderAuthInfo{
				{
					ProviderID: "anthropic-claude-code",
					Backend:    agent.BackendClaudeCode,
					Connected:  true,
					Source:     agent.CredentialSourceClaudeCLI,
				},
			},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := agent.IsAnyProviderConnected(c.providers); got != c.want {
				t.Errorf("IsAnyProviderConnected = %v, want %v", got, c.want)
			}
		})
	}
}

// IsBackendConnected must not let one backend's credential vouch for
// another — connecting GitHub Copilot (opencode) says nothing about
// whether claude-code can run.
func TestIsBackendConnected_DoesNotLeakAcrossBackends(t *testing.T) {
	t.Parallel()
	providers := []agent.ProviderAuthInfo{
		{ProviderID: "github-copilot", Backend: agent.BackendOpenCode, Connected: true},
		{ProviderID: "anthropic-api", Backend: agent.BackendClaudeCode},
		{ProviderID: "openai-codex", Backend: agent.BackendCodex},
	}
	if !agent.IsBackendConnected(providers, agent.BackendOpenCode) {
		t.Error("opencode has a connected provider but reads as disconnected")
	}
	for _, bt := range []agent.BackendType{agent.BackendClaudeCode, agent.BackendCodex} {
		if agent.IsBackendConnected(providers, bt) {
			t.Errorf("%s reads as connected off another backend's credential", bt)
		}
	}
}

// A catalog filtered to one backend carries no evidence about the
// others, so it must answer false rather than inferring absence.
func TestIsBackendConnected_FilteredCatalog(t *testing.T) {
	t.Parallel()
	claudeOnly := []agent.ProviderAuthInfo{
		{ProviderID: "anthropic-api", Backend: agent.BackendClaudeCode, Connected: true},
	}
	if agent.IsBackendConnected(claudeOnly, agent.BackendOpenCode) {
		t.Error("a claude-scoped catalog must not report opencode connected")
	}
}
