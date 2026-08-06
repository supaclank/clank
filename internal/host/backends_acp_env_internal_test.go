package host

import (
	"testing"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/agent/acp"
)

// envTestProfile is the minimum AdapterProfile validate() accepts, with
// the scoped Env the manager wraps.
func envTestProfile(scoped func(string) map[string]string) acp.AdapterProfile {
	return acp.AdapterProfile{
		ID:      "env-test",
		Backend: agent.BackendClaudeCode,
		Scope:   acp.ScopeHost,
		Command: func(string) (string, []string) { return "true", nil },
		Env:     scoped,
	}
}

// The whole reason ambient env lives on the manager rather than in a
// provider sink: opencode wires no envFn at all, so anything routed
// through SetEnvResolver never reaches it. GH_TOKEN has to.
func TestAmbientEnv_ReachesBackendWithNoProviderResolver(t *testing.T) {
	t.Parallel()

	m, err := NewACPBackendManager(envTestProfile(nil))
	if err != nil {
		t.Fatalf("NewACPBackendManager: %v", err)
	}
	m.SetAmbientEnvResolver(func() map[string]string {
		return map[string]string{"GH_TOKEN": "gho_ambient"}
	})

	env := m.profile.Env("")
	if got := env["GH_TOKEN"]; got != "gho_ambient" {
		t.Fatalf("GH_TOKEN = %q, want %q (ambient env must not depend on a provider resolver)", got, "gho_ambient")
	}
}

// Ambient env is merged first so a provider sink wins any key it also
// sets: sandbox env must never shadow a credential.
func TestAmbientEnv_ProviderResolverWinsCollision(t *testing.T) {
	t.Parallel()

	m, err := NewACPBackendManager(envTestProfile(func(string) map[string]string {
		return map[string]string{"SCOPED": "from-profile"}
	}))
	if err != nil {
		t.Fatalf("NewACPBackendManager: %v", err)
	}
	m.SetAmbientEnvResolver(func() map[string]string {
		return map[string]string{"GH_TOKEN": "gho_ambient", "SHARED": "from-ambient"}
	})
	m.SetEnvResolver(func() map[string]string {
		return map[string]string{"ANTHROPIC_API_KEY": "sk-provider", "SHARED": "from-provider"}
	})

	env := m.profile.Env("")
	for k, want := range map[string]string{
		"SCOPED":            "from-profile",
		"GH_TOKEN":          "gho_ambient",
		"ANTHROPIC_API_KEY": "sk-provider",
		"SHARED":            "from-provider",
	} {
		if got := env[k]; got != want {
			t.Errorf("env[%q] = %q, want %q", k, got, want)
		}
	}
}

// The resolver feeds the supervisor's env fingerprint, so an unset
// ambient source must resolve to nothing rather than to an empty-string
// GH_TOKEN — gh treats a set-but-empty GH_TOKEN as a credential and
// fails authentication instead of falling through to its other sources.
func TestAmbientEnv_UnsetResolvesToNoKey(t *testing.T) {
	t.Parallel()

	m, err := NewACPBackendManager(envTestProfile(nil))
	if err != nil {
		t.Fatalf("NewACPBackendManager: %v", err)
	}
	m.SetAmbientEnvResolver(func() map[string]string { return nil })

	if env := m.profile.Env(""); len(env) != 0 {
		t.Fatalf("env = %v, want empty (no ambient credential, no provider resolver)", env)
	}
}

// githubAgentEnv must tolerate a Service with no GitHub manager (home-dir
// resolution failed at construction). Resolvers run on the reconcile
// path, where a panic would take down the supervisor loop.
func TestGithubAgentEnv_NilManagerResolvesToNil(t *testing.T) {
	t.Parallel()

	s := &Service{}
	if env := s.githubAgentEnv(); env != nil {
		t.Fatalf("githubAgentEnv() = %v, want nil when the GitHub manager is unavailable", env)
	}
}
