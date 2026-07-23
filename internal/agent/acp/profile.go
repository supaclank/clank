package acp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/acksell/clank/internal/agent"
)

// AdapterProfile is the per-adapter variance consumed by the supervisor
// and conn layers: how to launch the process, at what scope, and with
// which environment. Turn-level variance (session/new _meta, mode maps,
// guidance strategy) is added by the backend slices that need it.
type AdapterProfile struct {
	// ID is a stable identifier used in logs and the software manifest.
	ID string
	// Backend is the clank backend type this profile serves.
	Backend agent.BackendType
	Scope   AdapterScope
	// Command returns the argv that launches the adapter for scopeDir
	// (empty for ScopeHost profiles).
	Command func(scopeDir string) (bin string, args []string)
	// Env returns credential/config env vars merged over the parent
	// environment at spawn. A change in the returned map restarts the
	// adapter on the next reconcile (see envFingerprint).
	Env func() map[string]string
	// SessionNewMeta builds session/new's _meta payload; guidance is the
	// assembled system prompt for fresh sessions ("" on resume). nil =
	// no meta (the adapter has no session-level injection channel).
	SessionNewMeta func(guidance string) map[string]any
	// ModeFor maps clank's permission mode onto the adapter's ACP mode
	// id. nil or !ok = the mode is not sent (opencode ignores modes).
	ModeFor func(mode agent.ClaudePermissionMode) (string, bool)
	// ModelOption maps a model override onto a session config option
	// (option id + value). nil = overrides are ignored for this adapter.
	ModelOption func(o agent.ModelOverride) (id, value string, ok bool)
}

// ScopeKey maps a session workDir onto the supervisor's process key.
func (p AdapterProfile) ScopeKey(workDir string) string {
	if p.Scope == ScopeHost {
		return ""
	}
	return workDir
}

func (p AdapterProfile) validate() error {
	if p.ID == "" || p.Backend == "" || p.Command == nil {
		return fmt.Errorf("acp: profile needs ID, Backend and Command")
	}
	return nil
}

// envFingerprint hashes the profile env so credential rotation is
// detectable: reconcile restarts any process whose spawn-time fingerprint
// no longer matches the current one.
func envFingerprint(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
