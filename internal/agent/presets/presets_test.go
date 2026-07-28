package presets_test

import (
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/presets"
)

var allBackends = []agent.BackendType{agent.BackendClaudeCode, agent.BackendCodex, agent.BackendOpenCode}

// Both environment sets ship a Default and a Plan preset per backend, and
// every config value is non-empty (values are advertised ids, measured
// against the pinned adapters — an empty value would be a data bug).
func TestBuiltinSets_CoverEveryBackend(t *testing.T) {
	t.Parallel()
	for name, set := range map[string][]presets.Preset{
		"sandbox":     presets.Sandbox(),
		"workstation": presets.Workstation(),
	} {
		byID := map[string]presets.Preset{}
		for _, p := range set {
			if err := p.Validate(); err != nil {
				t.Errorf("%s: %s: %v", name, p.ID, err)
			}
			if !p.Builtin {
				t.Errorf("%s: %s is not marked builtin", name, p.ID)
			}
			for k, v := range p.Config {
				if k == "" || v == "" {
					t.Errorf("%s: %s has empty config entry %q=%q", name, p.ID, k, v)
				}
			}
			byID[p.ID] = p
		}
		for _, bt := range allBackends {
			d, ok := byID[presets.BuiltinDefaultPrefix+string(bt)]
			if !ok {
				t.Errorf("%s: no Default preset for %s", name, bt)
				continue
			}
			// Every Default names a mode: the whole point is that no
			// session lands in an agent factory default nobody chose.
			if d.Config[agent.ConfigOptionMode] == "" {
				t.Errorf("%s: Default for %s carries no mode", name, bt)
			}
			if _, ok := byID[presets.BuiltinPlanPrefix+string(bt)]; !ok {
				t.Errorf("%s: no Plan preset for %s", name, bt)
			}
		}
	}
}

// The sets differ ONLY in the Default posture (sandbox permissive,
// workstation guarded); Plan is identical — planning is read-only by
// construction, environment notwithstanding.
func TestBuiltinSets_DifferOnlyInDefaultMode(t *testing.T) {
	t.Parallel()
	sb, ws := presets.Sandbox(), presets.Workstation()
	if len(sb) != len(ws) {
		t.Fatalf("set sizes differ: sandbox=%d workstation=%d", len(sb), len(ws))
	}
	sandboxModes := map[agent.BackendType]string{}
	for _, p := range sb {
		if p.ID == presets.BuiltinDefaultPrefix+string(p.Backend) {
			sandboxModes[p.Backend] = p.Config[agent.ConfigOptionMode]
		}
	}
	for _, p := range ws {
		if p.ID != presets.BuiltinDefaultPrefix+string(p.Backend) {
			continue
		}
		// opencode's build agent is its own default in both postures; the
		// host-scoped agents diverge.
		if p.Backend == agent.BackendOpenCode {
			if sandboxModes[p.Backend] != p.Config[agent.ConfigOptionMode] {
				t.Errorf("opencode Default should match across sets")
			}
		} else if sandboxModes[p.Backend] == p.Config[agent.ConfigOptionMode] {
			t.Errorf("%s: sandbox and workstation Default share mode %q — the posture split is the point", p.Backend, p.Config[agent.ConfigOptionMode])
		}
	}
}

// RequiredKeys is the create-time contract: exactly the Default preset's
// keys, and nil for a backend the built-ins don't know (no invented
// requirements for a future backend the host serves but we don't).
func TestRequiredKeys(t *testing.T) {
	t.Parallel()
	sb := presets.Sandbox()
	got := presets.RequiredKeys(sb, agent.BackendClaudeCode)
	want := map[string]bool{agent.ConfigOptionMode: true, "model": true, "effort": true}
	if len(got) != len(want) {
		t.Fatalf("claude required keys = %v, want mode/model/effort", got)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected required key %q", k)
		}
	}
	if keys := presets.RequiredKeys(sb, agent.BackendType("future-agent")); keys != nil {
		t.Errorf("unknown backend required keys = %v, want nil", keys)
	}
}

// EnvValue/Parse is the provisioner→host boundary: a lossless round trip,
// with Builtin forced on so a provisioner cannot ship mutable built-ins.
func TestEnvValueParseRoundTrip(t *testing.T) {
	t.Parallel()
	in := presets.Sandbox()
	in[0].Builtin = false // a provisioner "forgetting" the flag
	out, err := presets.Parse(presets.EnvValue(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("round trip lost presets: %d != %d", len(out), len(in))
	}
	for i := range out {
		if !out[i].Builtin {
			t.Errorf("%s: Parse must force Builtin", out[i].ID)
		}
		if out[i].ID != in[i].ID || out[i].Config[agent.ConfigOptionMode] != in[i].Config[agent.ConfigOptionMode] {
			t.Errorf("round trip mutated %s", in[i].ID)
		}
	}

	// "" is "not declared", distinct from declared-empty.
	if ps, err := presets.Parse(""); err != nil || ps != nil {
		t.Errorf("Parse(\"\") = %v, %v; want nil, nil", ps, err)
	}
	if _, err := presets.Parse("{not json"); err == nil {
		t.Error("malformed env must error, not boot with wrong defaults")
	}
	if _, err := presets.Parse(`[{"id":"x","name":"","backend":"codex","config":{"mode":"agent"}}]`); err == nil {
		t.Error("invalid preset in env must error")
	}
}

// A provisioner declaring a built-in outside the reserved prefix could
// later collide with a user preset claiming the same id — the store only
// guards ids that already look reserved.
func TestParse_RejectsUnreservedBuiltinID(t *testing.T) {
	t.Parallel()
	env := `[{"id":"custom-tool","name":"Custom","backend":"codex","config":{"mode":"agent"}}]`
	if _, err := presets.Parse(env); err == nil {
		t.Error("builtin id outside the reserved prefix must error")
	}
}

// Preset.ID rides DELETE /presets/{id} as a single path segment; a "/" or
// "\" in it would make the preset impossible to address for deletion.
func TestPreset_ValidateRejectsPathUnsafeID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"foo/bar", `foo\bar`, "foo bar", " foo", "foo "} {
		p := presets.Preset{ID: id, Name: "N", Backend: agent.BackendCodex, Config: map[string]string{"mode": "agent"}}
		if err := p.Validate(); err == nil {
			t.Errorf("Validate() with id %q = nil error, want rejection", id)
		}
	}
}

// A duplicate id would let RequiredKeys silently pick whichever entry it
// happens to scan first instead of failing the boot on a provisioner bug.
func TestParse_RejectsDuplicateIDs(t *testing.T) {
	t.Parallel()
	env := `[
		{"id":"builtin-default-codex","name":"Default","backend":"codex","config":{"mode":"agent"}},
		{"id":"builtin-default-codex","name":"Default2","backend":"codex","config":{"mode":"agent-full-access"}}
	]`
	if _, err := presets.Parse(env); err == nil {
		t.Error("duplicate builtin preset id must error")
	}
}

// A Default preset with no Config would make RequiredKeys return an empty
// required-key set — silently disabling create-time validation instead of
// failing loudly, the opposite of this package's "no fallbacks" contract.
func TestParse_RejectsDefaultPresetWithoutConfig(t *testing.T) {
	t.Parallel()
	env := `[{"id":"builtin-default-codex","name":"Default","backend":"codex","instructions":"be careful"}]`
	if _, err := presets.Parse(env); err == nil {
		t.Error("default preset without config must error")
	}
}
