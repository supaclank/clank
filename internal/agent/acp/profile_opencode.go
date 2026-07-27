package acp

import "github.com/acksell/clank/internal/agent"

// OpenCodeProfile serves the opencode backend through `opencode acp`, one
// process per project dir (the subcommand boots a full opencode server
// bound to its cwd). bin is the opencode executable — the user's own
// install by design: their binary, their state, no version skew clank can
// introduce. Credentials ride opencode's auth store, so Env is nil.
func OpenCodeProfile(bin string) AdapterProfile {
	return AdapterProfile{
		ID:      "opencode-acp",
		Backend: agent.BackendOpenCode,
		Scope:   ScopePerDir,
		Command: func(scopeDir string) (string, []string) {
			return bin, []string{"acp", "--cwd", scopeDir}
		},
		// build is opencode's own default agent in both postures — its
		// per-tool permission gates come from the repo's opencode config,
		// which is the user's own declared stance. Setting it is a no-op
		// guard against a repo that reorders its default.
		DefaultModes: PostureModes{Permissive: "build", Conservative: "build"},
		// Models are opencode's "model" config option.
		ModelOption: func(o agent.ModelOverride) (string, string, bool) {
			if o.ModelID == "" || o.ProviderID == "" {
				return "", "", false
			}
			return "model", o.ProviderID + "/" + o.ModelID, true
		},
	}
}
