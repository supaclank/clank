package launchconfig

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const validLaunchYAML = `default: web-app
previews:
  web-app:
    directory: web
    command: npm run dev -- --port "$PORT"
    env:
      __VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS: "${CLANK_PREVIEW_PUBLIC_HOSTNAME}"
    ready:
      path: /
`

func TestResolveProjectLaunch(t *testing.T) {
	t.Parallel()

	root := newLaunchRepo(t)
	writeProjectLaunch(t, root, validLaunchYAML)

	got, err := Resolve(filepath.Join(root, "web"), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "web-app" {
		t.Errorf("Name = %q, want web-app", got.Name)
	}
	if got.WorkDir != filepath.Join(root, "web") {
		t.Errorf("WorkDir = %q, want %q", got.WorkDir, filepath.Join(root, "web"))
	}
	if got.Command != `npm run dev -- --port "$PORT"` {
		t.Errorf("Command = %q", got.Command)
	}
	if got.Environment["__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS"] != "${CLANK_PREVIEW_PUBLIC_HOSTNAME}" {
		t.Errorf("Environment = %v", got.Environment)
	}
	if got.Ready.Path != "/" || got.Ready.ExpectedSubstring != "" {
		t.Errorf("Ready = %+v", got.Ready)
	}
	if got.Source.Path != filepath.Join(root, ProjectRelativePath) {
		t.Errorf("Source = %+v", got.Source)
	}
}

func TestResolveNamedLaunch(t *testing.T) {
	t.Parallel()

	root := newLaunchRepo(t)
	writeProjectLaunch(t, root, `default: web-app
previews:
  web-app:
    directory: web
    command: npm run dev -- --port "$PORT"
    ready:
      path: /
  admin:
    directory: admin
    command: pnpm dev --port "$PORT"
    ready:
      path: /healthz
      expect: ok
`)
	if err := os.Mkdir(filepath.Join(root, "admin"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve(root, "admin")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Name != "admin" || got.WorkDir != filepath.Join(root, "admin") {
		t.Fatalf("Resolve named = %+v", got)
	}
	if got.Ready.Path != "/healthz" || got.Ready.ExpectedSubstring != "ok" {
		t.Errorf("Ready = %+v", got.Ready)
	}
}

func TestResolveRejectsInvalidProjectConfig(t *testing.T) {
	t.Parallel()
	root := newLaunchRepo(t)
	writeProjectLaunch(t, root, "prevews: {}\n")

	_, err := Resolve(root, "")
	if err == nil || !strings.Contains(err.Error(), "field prevews not found") {
		t.Fatalf("Resolve: err = %v, want strict project-config error", err)
	}
}

func TestResolveMissingReturnsSetupPaths(t *testing.T) {
	root := newLaunchRepo(t)
	_, err := Resolve(root, "")
	var missing *NotFoundError
	if !errors.As(err, &missing) {
		t.Fatalf("Resolve: err = %T %v, want *NotFoundError", err, err)
	}
	if missing.Paths.Project != filepath.Join(root, ProjectRelativePath) {
		t.Errorf("project setup path = %q", missing.Paths.Project)
	}
}

func TestLaunchConfigValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		prepare func(*testing.T, string)
		wantErr string
	}{
		{name: "unknown field", yaml: strings.Replace(validLaunchYAML, "default:", "version: 1\ndefault:", 1), wantErr: "field version not found"},
		{name: "duplicate field", yaml: "default: web-app\n" + validLaunchYAML, wantErr: "field default already set"},
		{name: "missing default", yaml: strings.Replace(validLaunchYAML, "default: web-app\n", "", 1), wantErr: "default is required"},
		{name: "unknown default", yaml: strings.Replace(validLaunchYAML, "default: web-app", "default: missing", 1), wantErr: "default preview \"missing\" is not defined"},
		{name: "invalid name", yaml: strings.ReplaceAll(validLaunchYAML, "web-app", "web app"), wantErr: "invalid preview name"},
		{name: "reserved Expo service name", yaml: strings.ReplaceAll(validLaunchYAML, "web-app", "default"), wantErr: "reserved for Expo"},
		{name: "missing directory", yaml: strings.Replace(validLaunchYAML, "    directory: web\n", "", 1), wantErr: "directory is required"},
		{name: "directory escape", yaml: strings.Replace(validLaunchYAML, "directory: web", "directory: ../web", 1), wantErr: "local path"},
		{name: "missing directory on disk", yaml: strings.Replace(validLaunchYAML, "directory: web", "directory: missing", 1), wantErr: "preview directory"},
		{
			name: "symlink directory escape",
			yaml: strings.Replace(validLaunchYAML, "directory: web", "directory: outside", 1),
			prepare: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "escapes project root",
		},
		{name: "missing command", yaml: strings.Replace(validLaunchYAML, "    command: npm run dev -- --port \"$PORT\"\n", "", 1), wantErr: "command is required"},
		{name: "command ignores port", yaml: strings.Replace(validLaunchYAML, `npm run dev -- --port "$PORT"`, "npm run dev", 1), wantErr: "must consume $PORT"},
		{name: "different variable with port prefix", yaml: strings.Replace(validLaunchYAML, `npm run dev -- --port "$PORT"`, `npm run dev -- --port "$PORTER"`, 1), wantErr: "must consume $PORT"},
		{name: "invalid environment name", yaml: strings.Replace(validLaunchYAML, "__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS:", "INVALID-NAME:", 1), wantErr: "invalid environment variable name"},
		{name: "reserved port environment", yaml: strings.Replace(validLaunchYAML, "__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS:", "PORT:", 1), wantErr: "managed by Clank"},
		{name: "reserved public hostname environment", yaml: strings.Replace(validLaunchYAML, "__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS:", "CLANK_PREVIEW_PUBLIC_HOSTNAME:", 1), wantErr: "managed by Clank"},
		{name: "unknown environment placeholder", yaml: strings.Replace(validLaunchYAML, "${CLANK_PREVIEW_PUBLIC_HOSTNAME}", "${UNKNOWN}", 1), wantErr: "unsupported placeholder"},
		{name: "malformed environment placeholder", yaml: strings.Replace(validLaunchYAML, "${CLANK_PREVIEW_PUBLIC_HOSTNAME}", "${PORT:-5173}", 1), wantErr: "unsupported placeholder"},
		{name: "unterminated environment placeholder", yaml: strings.Replace(validLaunchYAML, "${CLANK_PREVIEW_PUBLIC_HOSTNAME}", "${PORT", 1), wantErr: "unterminated environment placeholder"},
		{name: "missing readiness path", yaml: strings.Replace(validLaunchYAML, "      path: /\n", "", 1), wantErr: "ready.path is required"},
		{name: "absolute readiness URL", yaml: strings.Replace(validLaunchYAML, "path: /", "path: http://127.0.0.1/", 1), wantErr: "absolute URL"},
		{name: "readiness query", yaml: strings.Replace(validLaunchYAML, "path: /", "path: /health?full=1", 1), wantErr: "query or fragment"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := newLaunchRepo(t)
			if tt.prepare != nil {
				tt.prepare(t, root)
			}
			writeProjectLaunch(t, root, tt.yaml)

			_, err := Resolve(root, "")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Resolve: err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRenderEnvironment(t *testing.T) {
	t.Parallel()

	got, err := RenderEnvironment(map[string]string{
		"APP_ORIGIN": "http://${CLANK_PREVIEW_PUBLIC_HOSTNAME}:${PORT}",
		"STATIC":     "literal",
	}, 5173, "preview-token.example.test")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"APP_ORIGIN": "http://preview-token.example.test:5173",
		"STATIC":     "literal",
	}
	if len(got) != len(want) {
		t.Fatalf("RenderEnvironment = %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("RenderEnvironment[%q] = %q, want %q", key, got[key], value)
		}
	}
}

func TestResolveRejectsUnknownName(t *testing.T) {
	t.Parallel()
	root := newLaunchRepo(t)
	writeProjectLaunch(t, root, validLaunchYAML)

	_, err := Resolve(root, "admin")
	if err == nil || !strings.Contains(err.Error(), `preview "admin" is not defined`) {
		t.Fatalf("Resolve: err = %v, want unknown-name error", err)
	}
}

func newLaunchRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-b", "main")
	if err := os.Mkdir(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeProjectLaunch(t *testing.T, root, content string) {
	t.Helper()
	path := filepath.Join(root, ProjectRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
