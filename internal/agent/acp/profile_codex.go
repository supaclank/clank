package acp

import "github.com/acksell/clank/internal/agent"

// Codex mode ids exposed by codex-acp (approval+sandbox presets).
const (
	codexModeAgent      = "agent"
	codexModeReadOnly   = "read-only"
	codexModeFullAccess = "agent-full-access"
)

// CodexProfile serves the codex backend through the codex-acp adapter
// (npm @agentclientprotocol/codex-acp) run as plain JS under the pinned
// bun. One process per host: the adapter keeps a single codex app-server
// child that multiplexes every session as a thread; cwd is a per-session
// session/new parameter. env carries CODEX_API_KEY (nil = let codex fall
// back to its own ChatGPT login in ~/.codex).
func CodexProfile(bunBin, adapterEntry string, env func() map[string]string) AdapterProfile {
	return AdapterProfile{
		ID:      "codex-acp",
		Backend: agent.BackendCodex,
		Scope:   ScopeHost,
		Command: func(string) (string, []string) {
			return bunBin, []string{adapterEntry}
		},
		Env: env,
		// codex has three approval/sandbox presets; clank's four modes
		// collapse onto them. default/acceptEdits both land on "agent"
		// (workspace-write with permission requests) — codex has no
		// finer split. Always set explicitly: never rely on the user's
		// config.toml (codex-acp #310 clobbers it anyway).
		ModeFor: func(mode agent.ClaudePermissionMode) (string, bool) {
			switch mode {
			case agent.ClaudePermDefault, agent.ClaudePermAcceptEdits:
				return codexModeAgent, true
			case agent.ClaudePermPlan:
				return codexModeReadOnly, true
			case agent.ClaudePermBypass:
				return codexModeFullAccess, true
			default:
				return "", false
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
