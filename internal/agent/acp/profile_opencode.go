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
	}
}
