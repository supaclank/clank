package acp

import "github.com/acksell/clank/internal/agent"

// HermesProfile serves the hermes backend through `hermes acp` on the
// user's own install (Nous Research's hermes-agent; official installer
// or pipx/uv from PyPI) — their binary, their state, no version skew
// clank can introduce. One process per host: the adapter multiplexes
// sessions with per-session cwd and persists them in ~/.hermes, serving
// session/list and session/load from that store. Credentials ride
// hermes' own auth surface (~/.hermes/.env, Nous Portal login, or its
// config.yaml provider), so Env is nil. Modes are agent-owned
// (default / accept_edits / dont_ask) and ride SessionModeState
// untranslated. Model selection is hermes-side (its /model command);
// clank overrides are ignored.
func HermesProfile(bin string) AdapterProfile {
	return AdapterProfile{
		ID:      "hermes-acp",
		Backend: agent.BackendHermes,
		Scope:   ScopeHost,
		Command: func(string) (string, []string) {
			return bin, []string{"acp"}
		},
	}
}
