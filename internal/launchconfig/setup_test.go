package launchconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestLaunchSchemaIsJSONSchema(t *testing.T) {
	t.Parallel()

	var schema map[string]any
	if err := json.Unmarshal([]byte(LaunchSchema()), &schema); err != nil {
		t.Fatalf("LaunchSchema: %v", err)
	}
	if v, ok := schema["$schema"].(string); !ok || v == "" {
		t.Fatal("LaunchSchema has no $schema declaration")
	}
}

func TestLaunchSchemaKeysMatchGoYAMLTypes(t *testing.T) {
	t.Parallel()

	var schema map[string]any
	if err := json.Unmarshal([]byte(LaunchSchema()), &schema); err != nil {
		t.Fatal(err)
	}
	defs := schema["$defs"].(map[string]any)
	tests := []struct {
		name       string
		typeOf     reflect.Type
		properties map[string]any
	}{
		{name: "file", typeOf: reflect.TypeOf(File{}), properties: schema["properties"].(map[string]any)},
		{name: "preview", typeOf: reflect.TypeOf(Preview{}), properties: defs["preview"].(map[string]any)["properties"].(map[string]any)},
		{name: "ready", typeOf: reflect.TypeOf(Ready{}), properties: defs["ready"].(map[string]any)["properties"].(map[string]any)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var yamlKeys []string
			for i := range tt.typeOf.NumField() {
				yamlKeys = append(yamlKeys, tt.typeOf.Field(i).Tag.Get("yaml"))
			}
			var schemaKeys []string
			for key := range tt.properties {
				schemaKeys = append(schemaKeys, key)
			}
			sort.Strings(yamlKeys)
			sort.Strings(schemaKeys)
			if !reflect.DeepEqual(yamlKeys, schemaKeys) {
				t.Fatalf("YAML keys = %v, schema keys = %v", yamlKeys, schemaKeys)
			}
		})
	}
}

func TestSetupTaskPromptIncludesProjectTargetAndContract(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(root, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claudeLaunch := `{"servers":[{"name":"web","command":"npm run dev -- --port 5173","autoPort":true}]}`
	if err := os.WriteFile(filepath.Join(claudeDir, "launch.json"), []byte(claudeLaunch), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := Paths{
		ProjectRoot: root,
		Project:     filepath.Join(root, ProjectRelativePath),
	}
	prompt, err := SetupTaskPrompt(paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"one-time setup task",
		"non-interactive",
		"Do not ask the user questions",
		"three minutes",
		paths.Project,
		"hot module replacement",
		"Fast Refresh",
		"Do not configure or launch API servers",
		"Do not use Docker or Podman",
		"immutable or frozen install mode",
		`must consume the literal shell variable $PORT or ${PORT}`,
		"CLANK_PREVIEW_PUBLIC_HOSTNAME",
		"CLANK_PREVIEW_PUBLIC_URL",
		"absent for local previews",
		"__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS",
		"allowedDevOrigins",
		"process.env.CLANK_PREVIEW_PUBLIC_HOSTNAME",
		"When the variable is absent, omit the Clank-specific setting",
		"preserve and merge any existing allowlist entries",
		"framework development-server configuration",
		"Do not change application runtime code",
		"optional env map",
		"Claude allocates a port",
		"Clank does not rewrite command arguments",
		claudeLaunch,
		LaunchSchema(),
		"Do not commit, push, or open a pull request",
	} {
		if !strings.Contains(prompt, required) {
			t.Errorf("SetupTaskPrompt missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"ask whether",
		"Start each configured command",
		"Write only that configuration file",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("SetupTaskPrompt unexpectedly contains %q", forbidden)
		}
	}
}

func TestReadClaudeLaunchReferenceRejectsSymlinkEscapingProjectRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	claudeDir := filepath.Join(root, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(claudeDir, "launch.json")); err != nil {
		t.Fatal(err)
	}

	if _, err := readClaudeLaunchReference(root); err == nil {
		t.Fatal("expected error for launch.json symlink escaping project root")
	}
}

func TestReadClaudeLaunchReferenceRejectsNonRegularFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	claudeDir := filepath.Join(root, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeDir, "launch.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := readClaudeLaunchReference(root); err == nil {
		t.Fatal("expected error when launch.json is a directory")
	}
}
