// Package acp adapts Agent Client Protocol (ACP) agents to clank's
// SessionBackend seam. An AdapterSupervisor spawns and supervises adapter
// processes (opencode acp, claude-agent-acp, codex-acp) and owns their
// stdio JSON-RPC connections; per-adapter variance lives in AdapterProfile
// values, so one implementation serves every ACP agent.
package acp

import "time"

// AdapterScope declares how many adapter processes a profile needs.
type AdapterScope int

const (
	// ScopeHost runs one adapter process for the whole host; sessions carry
	// their own cwd (codex-acp app-server, claude-agent-acp).
	ScopeHost AdapterScope = iota
	// ScopePerDir runs one adapter process per project directory
	// (opencode acp boots a full opencode server bound to its cwd).
	ScopePerDir
)

// modelConfigOptionID is the ACP session-config option that carries the
// model picker — matched on the semantic category, with the id as a
// fallback for agents that omit the category.
const modelConfigOptionID = "model"

// modeConfigOptionID is the session-config option carrying the mode
// picker for agents that advertise modes there instead of (or as well
// as) in SessionModeState — `opencode acp` uses only this channel.
const modeConfigOptionID = "mode"

const (
	// defaultReconcileEvery matches OpenCodeServerManager's cadence.
	defaultReconcileEvery = 5 * time.Second
	// spawnTimeout bounds adapter start incl. the initialize roundtrip.
	spawnTimeout = 30 * time.Second
	// stopGrace is how long a SIGINT'd adapter gets before SIGKILL.
	stopGrace = 3 * time.Second
)
