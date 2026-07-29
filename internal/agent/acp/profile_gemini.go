package acp

import "github.com/acksell/clank/internal/agent"

// GeminiProfile serves the gemini backend through gemini-cli's native ACP
// mode (`gemini --acp`, the bundled launcher run as plain JS under the
// pinned bun). One process per host: gemini-cli builds a fresh per-session
// config from session/new's cwd, so sessions multiplex one process. Auth
// is agent-owned — cached Google OAuth or GEMINI_API_KEY inherited from
// the parent environment — so Env is nil; an unauthenticated session/new
// fails with a clear -32000 instead of wedging the adapter. Modes are
// agent-owned (default / autoEdit / yolo / plan) and ride
// SessionModeState untranslated. Model selection rides the models session
// state, which clank doesn't consume — overrides are ignored.
func GeminiProfile(bunBin, geminiEntry string) AdapterProfile {
	return AdapterProfile{
		ID:      "gemini-acp",
		Backend: agent.BackendGemini,
		Scope:   ScopeHost,
		Command: func(string) (string, []string) {
			return bunBin, []string{geminiEntry, "--acp"}
		},
	}
}
