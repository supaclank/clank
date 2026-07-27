package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/agent/presets"
)

func userPreset(id string) presets.Preset {
	return presets.Preset{
		ID: id, Name: "Review", Backend: agent.BackendClaudeCode,
		Config: map[string]string{agent.ConfigOptionMode: "plan", "effort": "max"},
	}
}

func TestPresetStore_CRUDRoundTripsAcrossReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := newPresetStore(dir)
	if err != nil {
		t.Fatalf("newPresetStore: %v", err)
	}
	if err := s.Put(userPreset("review")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reopened, err := newPresetStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.List()
	if len(got) != 1 || got[0].ID != "review" || got[0].Config["effort"] != "max" {
		t.Fatalf("List after reopen = %+v", got)
	}
	if err := reopened.Delete("review"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := reopened.Delete("review"); err == nil {
		t.Fatal("deleting a missing preset must error (fail fast), not no-op")
	}
}

// Built-in ids are reserved and built-ins are immutable — a user "editing"
// one duplicates it under a new id instead.
func TestPresetStore_RejectsBuiltinWrites(t *testing.T) {
	t.Parallel()
	s, err := newPresetStore(t.TempDir())
	if err != nil {
		t.Fatalf("newPresetStore: %v", err)
	}
	p := userPreset(presets.BuiltinDefaultPrefix + "claude-code")
	if err := s.Put(p); err == nil {
		t.Fatal("writing over a builtin id must fail")
	}
	q := userPreset("mine")
	q.Builtin = true
	if err := s.Put(q); err == nil {
		t.Fatal("a user preset claiming builtin must fail")
	}
}

// The store is user data, not a cache: a corrupt file refuses to load
// rather than silently starting empty and overwriting on the next save.
func TestPresetStore_CorruptFileRefusesToLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "presets.json"), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newPresetStore(dir); err == nil {
		t.Fatal("corrupt presets.json must fail loudly, not clobber user data")
	}
}

// Put rejects a builtin-flagged/builtin-prefixed preset, so the on-disk
// load path must enforce the same invariant — a hand-edited presets.json
// smuggling one in must not silently join the user store.
func TestPresetStore_RejectsForgedBuiltinOnLoad(t *testing.T) {
	t.Parallel()

	t.Run("builtin flag", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := userPreset("mine")
		p.Builtin = true
		writePresetsJSON(t, dir, p)
		if _, err := newPresetStore(dir); err == nil {
			t.Fatal("a loaded preset with Builtin=true must fail, not join the user store")
		}
	})

	t.Run("builtin id prefix", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writePresetsJSON(t, dir, userPreset(presets.BuiltinDefaultPrefix+"claude-code"))
		if _, err := newPresetStore(dir); err == nil {
			t.Fatal("a loaded preset claiming a builtin id must fail, not join the user store")
		}
	})

	t.Run("invalid preset", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writePresetsJSON(t, dir, presets.Preset{ID: "mine"}) // missing Name/Backend/Config
		if _, err := newPresetStore(dir); err == nil {
			t.Fatal("a loaded preset failing Validate must fail, not join the user store")
		}
	})
}

func writePresetsJSON(t *testing.T, dir string, ps ...presets.Preset) {
	t.Helper()
	raw, err := json.Marshal(ps)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "presets.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The create-time contract: required keys are the backend's built-in
// Default preset keys; missing ones fail with every gap named. The host
// fills nothing in.
func TestValidateCreateConfig(t *testing.T) {
	t.Parallel()
	s := &Service{builtinPresets: presets.Sandbox()}

	if err := s.ValidateCreateConfig(agent.BackendClaudeCode, map[string]string{
		agent.ConfigOptionMode: "bypassPermissions", "model": "default", "effort": "default",
	}); err != nil {
		t.Fatalf("complete config rejected: %v", err)
	}

	err := s.ValidateCreateConfig(agent.BackendClaudeCode, map[string]string{agent.ConfigOptionMode: "plan"})
	if err == nil {
		t.Fatal("missing model+effort must fail")
	}
	if !strings.Contains(err.Error(), "effort") || !strings.Contains(err.Error(), "model") {
		t.Errorf("error must name every missing key, got: %v", err)
	}

	// An empty-string value is as missing as an absent key — "" is the
	// old "no change" sentinel and must not satisfy a requirement.
	if err := s.ValidateCreateConfig(agent.BackendOpenCode, map[string]string{agent.ConfigOptionMode: ""}); err == nil {
		t.Fatal("empty mode value must fail for opencode")
	}

	// A backend the built-ins don't know imposes no invented requirements.
	if err := s.ValidateCreateConfig(agent.BackendType("future-agent"), nil); err != nil {
		t.Fatalf("unknown backend must not be blocked: %v", err)
	}
}
