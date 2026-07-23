package acp

import (
	"os"

	"github.com/acksell/clank/internal/agent"
)

// ClaudeProfile serves the claude-code backend through claude-agent-acp
// run as plain JS under the pinned bun. One process per host — the
// adapter spawns one Claude CLI per session internally, and the Agent
// SDK's bundled native CLI is the pinned agent. Guidance rides
// session/new's _meta.systemPrompt as a preset append (the adapter
// forwards it to the SDK). Credentials arrive via the manager's env
// resolver (CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY); running as
// root (sprites) additionally needs IS_SANDBOX=1 or the adapter blocks
// bypassPermissions.
func ClaudeProfile(bunBin, adapterEntry string, env func(string) map[string]string) AdapterProfile {
	if env == nil {
		env = func(string) map[string]string {
			if os.Geteuid() == 0 {
				return map[string]string{"IS_SANDBOX": "1"}
			}
			return nil
		}
	}
	return AdapterProfile{
		ID:      "claude-agent-acp",
		Backend: agent.BackendClaudeCode,
		Scope:   ScopeHost,
		Command: func(string) (string, []string) {
			return bunBin, []string{adapterEntry}
		},
		Env: env,
		SessionNewMeta: func(guidance string) map[string]any {
			if guidance == "" {
				return nil
			}
			// Merged onto the adapter's claude_code preset defaults —
			// the same semantics as the bespoke --append-system-prompt.
			return map[string]any{
				"systemPrompt": map[string]any{"append": guidance},
			}
		},
		ModelOption: func(o agent.ModelOverride) (string, string, bool) {
			if o.ModelID == "" {
				return "", "", false
			}
			return "model", o.ModelID, true
		},
	}
}
