package daemoncli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/acksell/clank/internal/host"
	"github.com/acksell/clank/pkg/provisioner"
)

// Tests for builtinTemplates, the env→config edge feeding every
// provisioner's builtin create-project catalog. None are parallel:
// t.Setenv is incompatible with t.Parallel.

// A daemon with no CLANK_TEMPLATES must still serve a create-project
// catalog — laptop users get the Expo starter with zero config.
func TestBuiltinTemplates_UnsetServesDefaultCatalog(t *testing.T) {
	t.Setenv("CLANK_TEMPLATES", "") // register restore, then truly unset
	os.Unsetenv("CLANK_TEMPLATES")

	got, err := builtinTemplates()
	if err != nil {
		t.Fatalf("builtinTemplates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("default catalog has %d entries, want 1: %+v", len(got), got)
	}
	if got[0].DisplayName != defaultTemplateDisplayName || got[0].CloneURL != defaultTemplateCloneURL {
		t.Errorf("default catalog = %+v, want {%s %s}", got[0], defaultTemplateDisplayName, defaultTemplateCloneURL)
	}
}

// Env passthroughs (docker-compose "${CLANK_TEMPLATES:-}") deliver ""
// for host-unset vars; that must mean "default", not "disabled".
func TestBuiltinTemplates_EmptyEnvServesDefaultCatalog(t *testing.T) {
	t.Setenv("CLANK_TEMPLATES", "")

	got, err := builtinTemplates()
	if err != nil {
		t.Fatalf("builtinTemplates: %v", err)
	}
	if len(got) != 1 || got[0].CloneURL != defaultTemplateCloneURL {
		t.Errorf("empty env yielded %+v, want the default catalog", got)
	}
}

// An explicit empty array is the operator's off switch: no builtin
// templates, and no silent resurrection of the default.
func TestBuiltinTemplates_EmptyArrayDisables(t *testing.T) {
	t.Setenv("CLANK_TEMPLATES", "[]")

	got, err := builtinTemplates()
	if err != nil {
		t.Fatalf("builtinTemplates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("CLANK_TEMPLATES=[] yielded %+v, want no templates", got)
	}
}

// A configured catalog replaces the default outright (no merging).
func TestBuiltinTemplates_EnvReplacesDefault(t *testing.T) {
	t.Setenv("CLANK_TEMPLATES", `[{"display_name":"My starter","clone_url":"https://templates.example/starter.git"}]`)

	got, err := builtinTemplates()
	if err != nil {
		t.Fatalf("builtinTemplates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("catalog has %d entries, want 1: %+v", len(got), got)
	}
	if got[0].DisplayName != "My starter" || got[0].CloneURL != "https://templates.example/starter.git" {
		t.Errorf("catalog = %+v, want the configured entry", got[0])
	}
	if got[0].CloneURL == defaultTemplateCloneURL {
		t.Error("configured catalog still contains the default entry")
	}
}

func TestBuiltinTemplates_InvalidJSONErrors(t *testing.T) {
	t.Setenv("CLANK_TEMPLATES", "{not json")

	if _, err := builtinTemplates(); err == nil {
		t.Fatal("builtinTemplates accepted invalid JSON")
	}
}

// The default catalog must survive the daemon→clank-host wire: marshal
// via provisioner.TemplatesEnvValue (what --templates-json carries),
// decode as internal/host.Template (what clank-host parses).
func TestDefaultTemplates_WireCompatibleWithHost(t *testing.T) {
	t.Parallel()
	raw := provisioner.TemplatesEnvValue(defaultTemplates())
	if raw == "" {
		t.Fatal("default catalog marshals to empty — clank-host would receive no flag")
	}
	var hostSide []host.Template
	if err := json.Unmarshal([]byte(raw), &hostSide); err != nil {
		t.Fatalf("clank-host side failed to parse the default catalog: %v", err)
	}
	if len(hostSide) != 1 || hostSide[0].DisplayName != defaultTemplateDisplayName || hostSide[0].CloneURL != defaultTemplateCloneURL {
		t.Errorf("host-side catalog = %+v, want {%s %s}", hostSide, defaultTemplateDisplayName, defaultTemplateCloneURL)
	}
}
