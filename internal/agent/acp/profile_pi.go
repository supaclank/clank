package acp

import "github.com/acksell/clank/internal/agent"

// PiProfile serves the pi backend through the pi-acp adapter (npm
// pi-acp, the community adapter listed in the ACP registry) run as
// plain JS under the pinned bun; the adapter spawns the PINNED pi CLI
// per session via PI_ACP_PI_COMMAND (a bun shim, see acptools).
//
// One process per project dir, NOT per host: the adapter keeps at most
// one live `pi --mode rpc` child per connection and kills the others on
// every session/new and session/load (older sessions respawn on
// demand), so co-hosting many active sessions on one connection lets a
// new session cancel another's in-flight turn. Per-dir processes bound
// that blast radius to sessions sharing a worktree — clank's dominant
// pattern is one worktree per session. session/list is cwd-filtered,
// matching per-dir discovery.
//
// pi has NO permission system for its core tools (read/write/edit/
// bash run unprompted; session/request_permission only bridges
// extension UI dialogs), so permission prompts never fire. Its ACP
// session modes are pi thinking levels (off…xhigh). Credentials ride
// pi's own store (~/.pi/agent/auth.json, env keys, models.json), so
// Env carries only the wrapper pointer.
func PiProfile(bunBin, adapterEntry, piWrapper string) AdapterProfile {
	return AdapterProfile{
		ID:      "pi-acp",
		Backend: agent.BackendPi,
		Scope:   ScopePerDir,
		Command: func(string) (string, []string) {
			return bunBin, []string{adapterEntry}
		},
		Env: func(string) map[string]string {
			return map[string]string{"PI_ACP_PI_COMMAND": piWrapper}
		},
		// pi advertises a "model" select whose value ids are
		// "provider/model" — same shape as opencode.
		ModelOption: func(o agent.ModelOverride) (string, string, bool) {
			if o.ModelID == "" || o.ProviderID == "" {
				return "", "", false
			}
			return "model", o.ProviderID + "/" + o.ModelID, true
		},
	}
}
