package preview

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/launchconfig"
	"github.com/acksell/clank/pkg/preview/tokens"
)

func TestResolveLaunchPreservesExpoDetection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePreviewFiles(t, dir, map[string]string{
		"package.json": `{"dependencies":{"expo":"~55.0.0"}}`,
		"app.json":     `{"expo":{"name":"app"}}`,
	})

	got, err := resolveLaunch(dir, "")
	if err != nil {
		t.Fatalf("resolveLaunch: %v", err)
	}
	if got.Spec.Kind != KindExpo || got.ServiceName != tokens.DefaultServiceName || got.WorkDir != dir {
		t.Fatalf("resolveLaunch = %+v, want detected Expo", got)
	}
}

func TestResolveLaunchRequiresConfigForWeb(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	dir := t.TempDir()
	writePreviewFiles(t, dir, map[string]string{
		"package.json": `{"devDependencies":{"vite":"^7.0.0"}}`,
	})

	_, err := resolveLaunch(dir, "")
	if !errors.Is(err, ErrSetupRequired) {
		t.Fatalf("resolveLaunch: err = %v, want ErrSetupRequired", err)
	}
	var setup *SetupRequiredError
	if !errors.As(err, &setup) {
		t.Fatalf("resolveLaunch: err = %T, want *SetupRequiredError", err)
	}
	if setup.ProjectConfigPath != filepath.Join(realPreviewPath(t, dir), ".clank", "launch.yaml") {
		t.Errorf("ProjectConfigPath = %q", setup.ProjectConfigPath)
	}
	for _, required := range []string{
		"one-time setup task",
		"non-interactive",
		setup.ProjectConfigPath,
		launchconfig.LaunchSchema(),
	} {
		if !strings.Contains(setup.Prompt, required) {
			t.Errorf("Prompt missing %q", required)
		}
	}
	if strings.Contains(setup.Prompt, "Run `clank preview`") {
		t.Errorf("Prompt still redirects non-CLI clients to the CLI: %q", setup.Prompt)
	}
}

func TestResolveLaunchUsesDefaultConfigEntry(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	dir := t.TempDir()
	writePreviewLaunchConfig(t, dir, `default: web
previews:
  web:
    directory: app
    command: npm run dev -- --port "$PORT"
    env:
      __VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS: "${CLANK_PREVIEW_PUBLIC_HOSTNAME}"
    ready:
      path: /healthz
      expect: ready
`)
	if err := os.Mkdir(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveLaunch(dir, "")
	if err != nil {
		t.Fatalf("resolveLaunch: %v", err)
	}
	if got.Spec.Kind != KindWeb || got.ServiceName != "web" || got.WorkDir != filepath.Join(realPreviewPath(t, dir), "app") {
		t.Fatalf("resolveLaunch = %+v", got)
	}
	if len(got.Spec.CmdTemplate) != 3 || got.Spec.CmdTemplate[0] != "sh" || got.Spec.CmdTemplate[1] != "-c" || got.Spec.CmdTemplate[2] != `npm run dev -- --port "$PORT"` {
		t.Errorf("CmdTemplate = %q", got.Spec.CmdTemplate)
	}
	if got.Spec.ReadyProbe.Path != "/healthz" || got.Spec.ReadyProbe.ExpectedSubstr != "ready" {
		t.Errorf("ReadyProbe = %+v", got.Spec.ReadyProbe)
	}
	if got.Spec.Environment["__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS"] != "${CLANK_PREVIEW_PUBLIC_HOSTNAME}" {
		t.Errorf("Environment = %v", got.Spec.Environment)
	}
}

func TestResolveLaunchWrapsSetupTaskPromptFailure(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	dir := t.TempDir()
	writePreviewFiles(t, dir, map[string]string{
		"package.json": `{"devDependencies":{"vite":"^7.0.0"}}`,
	})
	claudeDir := filepath.Join(dir, ".claude")
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

	_, err := resolveLaunch(dir, "")
	if !errors.Is(err, ErrInvalidLaunchConfig) {
		t.Fatalf("resolveLaunch: err = %v, want ErrInvalidLaunchConfig", err)
	}
	if !strings.Contains(err.Error(), "escapes project root") {
		t.Fatalf("resolveLaunch: err = %v, want the SetupTaskPrompt failure cause preserved in the chain", err)
	}
}

func realPreviewPath(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestResolveLaunchNamedConfigOverridesExpoDefault(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	dir := t.TempDir()
	writePreviewFiles(t, dir, map[string]string{
		"package.json": `{"dependencies":{"expo":"~55.0.0"}}`,
		"app.json":     `{"expo":{"name":"app"}}`,
	})
	writePreviewLaunchConfig(t, dir, `default: web
previews:
  web:
    directory: .
    command: npm run web -- --port "$PORT"
    ready:
      path: /
`)

	got, err := resolveLaunch(dir, "web")
	if err != nil {
		t.Fatalf("resolveLaunch: %v", err)
	}
	if got.Spec.Kind != KindWeb || got.ServiceName != "web" {
		t.Fatalf("resolveLaunch = %+v, want named web config", got)
	}

	expo, err := resolveLaunch(dir, "")
	if err != nil {
		t.Fatalf("resolveLaunch default: %v", err)
	}
	if expo.Spec.Kind != KindExpo {
		t.Fatalf("resolveLaunch default kind = %q, want Expo detection", expo.Spec.Kind)
	}
}

func writePreviewLaunchConfig(t *testing.T, root, content string) {
	t.Helper()
	writePreviewFiles(t, root, map[string]string{".clank/launch.yaml": content})
}

func writePreviewFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
