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
	if schema["$schema"] == "" {
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

func TestSetupTaskPromptIncludesSelectedTargetAndContract(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
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
		Host:        "/host/preview-launch/project.yaml",
	}
	prompt, err := SetupTaskPrompt(paths, ScopeHost)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"one-time setup task",
		"non-interactive",
		"Do not ask the user questions",
		"three minutes",
		filepath.Join(root, SetupRelativePath),
		paths.Host,
		"hot module replacement",
		"Fast Refresh",
		"Do not configure or launch API servers",
		"Do not use Docker or Podman",
		"immutable or frozen install mode",
		`must consume the literal shell variable $PORT or ${PORT}`,
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
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("SetupTaskPrompt unexpectedly contains %q", forbidden)
		}
	}
}

func TestSetupTaskPromptWritesProjectConfigWhenShared(t *testing.T) {
	t.Parallel()

	paths := Paths{
		ProjectRoot: t.TempDir(),
		Project:     "/work/project/.clank/launch.yaml",
		Host:        "/host/preview-launch/project.yaml",
	}
	prompt, err := SetupTaskPrompt(paths, ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `Write the configuration to the shared project file "/work/project/.clank/launch.yaml"`) {
		t.Fatalf("SetupTaskPrompt did not select project storage:\n%s", prompt)
	}
	if strings.Contains(prompt, SetupRelativePath) {
		t.Fatalf("project setup prompt mentions private staging path:\n%s", prompt)
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

func TestSetupTaskPromptRejectsUnknownScope(t *testing.T) {
	t.Parallel()

	_, err := SetupTaskPrompt(Paths{ProjectRoot: t.TempDir()}, Scope("other"))
	if err == nil || !strings.Contains(err.Error(), "unknown launch config scope") {
		t.Fatalf("SetupTaskPrompt error = %v", err)
	}
}

func TestSetupPromptDirectsInteractiveOneTimeSetup(t *testing.T) {
	t.Parallel()

	prompt := SetupPrompt(Paths{ProjectRoot: "/work/project"})
	if !strings.Contains(prompt, "one-time") || !strings.Contains(prompt, "clank preview") {
		t.Fatalf("SetupPrompt = %q", prompt)
	}
}
