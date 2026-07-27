package acp

import "github.com/acksell/clank/internal/agent"

// CodexProfile serves the codex backend through the codex-acp adapter
// (npm @agentclientprotocol/codex-acp) run as plain JS under the pinned
// bun. One process per host: the adapter keeps a single codex app-server
// child that multiplexes every session as a thread; cwd is a per-session
// session/new parameter. env carries CODEX_API_KEY (nil = let codex fall
// back to its own ChatGPT login in ~/.codex). Modes are agent-owned:
// codex advertises its approval/sandbox presets (read-only / agent /
// agent-full-access) and clank passes the chosen id straight through.
func CodexProfile(bunBin, adapterEntry string, env func(string) map[string]string) AdapterProfile {
	return AdapterProfile{
		ID:      "codex-acp",
		Backend: agent.BackendCodex,
		Scope:   ScopeHost,
		Command: func(string) (string, []string) {
			return bunBin, []string{adapterEntry}
		},
		Env: env,
		// codex advertises read-only/agent/agent-full-access. Full access
		// also lifts codex's own inner sandbox (network etc.), which is
		// redundant inside a disposable clank sandbox; `agent`
		// (workspace-write) is the conservative stance.
		DefaultModes: PostureModes{
			Permissive:   "agent-full-access",
			Conservative: "agent",
		},
		ModelOption: func(o agent.ModelOverride) (string, string, bool) {
			if o.ModelID == "" {
				return "", "", false
			}
			return "model", o.ModelID, true
		},
	}
}
