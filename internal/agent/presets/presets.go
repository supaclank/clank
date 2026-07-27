// Package presets defines agent presets: named bundles of session config
// values (mode, model, effort, …) a client applies when creating or
// steering a session.
//
// The host serves presets and stores user-created ones; it NEVER applies
// them to a request — the client is the only thing that sends config
// (policy lives with the caller). Built-ins arrive via
// $CLANK_BUILTIN_PRESETS, serialized from the typed sets below by whatever
// provisioned the host — the one thing that knows the environment's blast
// radius. Same pattern as CLANK_TEMPLATES: Go values in one file,
// marshaled only at the process boundary.
//
// Every config value below is a value id the agent ADVERTISES (measured
// against claude-agent-acp 0.61.0, codex-acp 1.1.7, opencode 1.17.18 —
// re-probe on adapter bumps). Keys a backend cannot express truthfully are
// absent: codex and opencode advertise no "default" alias for model, so
// their presets leave the model knob untouched and the agent's own config
// governs (visible to clients as the option's current value).
package presets

import (
	"encoding/json"
	"fmt"

	"github.com/acksell/clank/internal/agent"
)

// Preset is one named config bundle for one backend.
type Preset struct {
	// ID is stable and unique per host. Built-in ids are prefixed
	// "builtin-" and reserved: user presets may not claim them.
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Backend agent.BackendType `json:"backend"`
	// Config is applied verbatim by clients as StartRequest/
	// SendMessageOpts config: agent-advertised option id → value id.
	Config map[string]string `json:"config"`
	// Instructions is reserved for preset-carried system-prompt text
	// (rides the per-adapter guidance channels, not config options).
	// Serialized when present so older hosts round-trip it untouched.
	Instructions string `json:"instructions,omitempty"`
	// Builtin marks host-shipped presets: immutable, undeletable,
	// duplicate-to-edit.
	Builtin bool `json:"builtin,omitempty"`
}

// Validate checks the fields the store and the wire both rely on.
func (p Preset) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("preset id is required")
	}
	if p.Name == "" {
		return fmt.Errorf("preset name is required")
	}
	if p.Backend == "" {
		return fmt.Errorf("preset backend is required")
	}
	if len(p.Config) == 0 && p.Instructions == "" {
		return fmt.Errorf("preset must carry config or instructions")
	}
	return nil
}

// Builtin preset IDs. The Default preset doubles as the create-time
// validation contract: its config KEYS are the required keys for that
// backend (see RequiredKeys).
const (
	BuiltinDefaultPrefix = "builtin-default-"
	BuiltinPlanPrefix    = "builtin-plan-"
)

// Sandbox returns the built-in presets for disposable environments (cloud
// sandboxes): run without permission prompts.
func Sandbox() []Preset {
	return withPlans([]Preset{
		defaultPreset(agent.BackendClaudeCode, map[string]string{
			agent.ConfigOptionMode: string(agent.ClaudePermBypass),
			"model":                "default",
			"effort":               "default",
		}),
		defaultPreset(agent.BackendCodex, map[string]string{
			agent.ConfigOptionMode: "agent-full-access",
			"collaboration_mode":   "default",
		}),
		defaultPreset(agent.BackendOpenCode, map[string]string{
			agent.ConfigOptionMode: "build",
		}),
	})
}

// Workstation returns the built-in presets for machines with real data (a
// laptop, a self-hosted box): the agent's guarded stance, no prompt spam.
// The default set when a host declares nothing.
func Workstation() []Preset {
	return withPlans([]Preset{
		defaultPreset(agent.BackendClaudeCode, map[string]string{
			agent.ConfigOptionMode: string(agent.ClaudePermAuto),
			"model":                "default",
			"effort":               "default",
		}),
		defaultPreset(agent.BackendCodex, map[string]string{
			agent.ConfigOptionMode: "agent",
			"collaboration_mode":   "default",
		}),
		defaultPreset(agent.BackendOpenCode, map[string]string{
			agent.ConfigOptionMode: "build",
		}),
	})
}

func defaultPreset(bt agent.BackendType, cfg map[string]string) Preset {
	return Preset{
		ID:      BuiltinDefaultPrefix + string(bt),
		Name:    "Default",
		Backend: bt,
		Config:  cfg,
		Builtin: true,
	}
}

// withPlans appends the Plan preset per backend. Identical across
// environment sets: planning is read-only by construction.
func withPlans(defaults []Preset) []Preset {
	plans := []Preset{
		{
			ID: BuiltinPlanPrefix + string(agent.BackendClaudeCode), Name: "Plan",
			Backend: agent.BackendClaudeCode, Builtin: true,
			Config: map[string]string{
				agent.ConfigOptionMode: string(agent.ClaudePermPlan),
				"model":                "default",
				"effort":               "default",
			},
		},
		{
			ID: BuiltinPlanPrefix + string(agent.BackendCodex), Name: "Plan",
			Backend: agent.BackendCodex, Builtin: true,
			Config: map[string]string{
				agent.ConfigOptionMode: "read-only",
				"collaboration_mode":   "plan",
			},
		},
		{
			ID: BuiltinPlanPrefix + string(agent.BackendOpenCode), Name: "Plan",
			Backend: agent.BackendOpenCode, Builtin: true,
			Config: map[string]string{
				agent.ConfigOptionMode: "plan",
			},
		},
	}
	return append(defaults, plans...)
}

// RequiredKeys returns the create-time required config keys per backend:
// exactly the keys of that backend's built-in Default preset. Data-defined
// strictness — a create missing any of them fails loudly (the host never
// fills values in), and a client following the preset flow satisfies it by
// construction.
func RequiredKeys(builtins []Preset, bt agent.BackendType) []string {
	for _, p := range builtins {
		if p.Backend == bt && p.ID == BuiltinDefaultPrefix+string(bt) {
			keys := make([]string, 0, len(p.Config))
			for k := range p.Config {
				keys = append(keys, k)
			}
			return keys
		}
	}
	return nil
}

// EnvValue serializes presets for $CLANK_BUILTIN_PRESETS (provisioner →
// host boundary). Empty input serializes to "" so an unset env stays
// distinguishable from an explicit empty list.
func EnvValue(ps []Preset) string {
	if len(ps) == 0 {
		return ""
	}
	b, _ := json.Marshal(ps)
	return string(b)
}

// Parse decodes $CLANK_BUILTIN_PRESETS. "" means "not declared" — callers
// fall back to Workstation(). Every entry is validated and forced Builtin,
// so a provisioner cannot ship mutable or malformed built-ins.
func Parse(env string) ([]Preset, error) {
	if env == "" {
		return nil, nil
	}
	var ps []Preset
	if err := json.Unmarshal([]byte(env), &ps); err != nil {
		return nil, fmt.Errorf("parse builtin presets: %w", err)
	}
	for i := range ps {
		if err := ps[i].Validate(); err != nil {
			return nil, fmt.Errorf("builtin preset %d: %w", i, err)
		}
		ps[i].Builtin = true
	}
	return ps, nil
}
