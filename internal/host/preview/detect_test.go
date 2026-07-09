package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Detect's job is small: classify a directory as Expo-previewable or
// not. The table doubles as the spec for what we mean by "Expo
// project" — adding a new positive or negative row IS the place to
// document an edge case.
func TestDetect(t *testing.T) {
	t.Parallel()

	type fixture struct {
		// name is the subtest name.
		name string
		// files maps "relative/path" → file contents to create under
		// the test's temp dir. nil contents means "don't create".
		files map[string]string
		// want is what Detect should return: nil for not-previewable,
		// &Spec{...} for previewable. We compare the spec's Kind and
		// ReadyPattern; CmdTemplate is checked separately because it
		// uses an opaque %d placeholder.
		want *Spec
	}

	cases := []fixture{
		{
			name: "expo via dependencies + app.json",
			files: map[string]string{
				"package.json": `{"dependencies":{"expo":"~50.0.0"}}`,
				"app.json":     `{"expo":{"name":"x"}}`,
			},
			want: &Spec{Kind: KindExpo, ReadyProbe: expoReadyProbe},
		},
		{
			name: "expo via devDependencies + app.config.js",
			files: map[string]string{
				"package.json":  `{"devDependencies":{"expo":"~50.0.0"}}`,
				"app.config.js": "export default { expo: {} };",
			},
			want: &Spec{Kind: KindExpo, ReadyProbe: expoReadyProbe},
		},
		{
			name: "expo via app.config.ts",
			files: map[string]string{
				"package.json":  `{"dependencies":{"expo":"^51.0.0"}}`,
				"app.config.ts": "export default { expo: {} };",
			},
			want: &Spec{Kind: KindExpo, ReadyProbe: expoReadyProbe},
		},
		{
			name:  "no package.json",
			files: map[string]string{},
			want:  nil,
		},
		{
			name: "package.json without expo dep",
			files: map[string]string{
				"package.json": `{"dependencies":{"react":"^18"}}`,
				"app.json":     `{"name":"x"}`,
			},
			want: nil,
		},
		{
			name: "expo dep but no app config",
			files: map[string]string{
				"package.json": `{"dependencies":{"expo":"~50.0.0"}}`,
			},
			want: nil,
		},
		{
			name: "malformed package.json",
			files: map[string]string{
				"package.json": `{"dependencies":{`, // truncated JSON
				"app.json":     `{"expo":{}}`,
			},
			want: nil,
		},
		{
			name: "empty package.json",
			files: map[string]string{
				"package.json": `{}`,
				"app.json":     `{"expo":{}}`,
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for rel, content := range tc.files {
				path := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", path, err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
			}

			got, err := Detect(dir)
			if err != nil {
				t.Fatalf("Detect: unexpected error %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("Detect: want nil spec, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Detect: want non-nil spec, got nil")
			}
			if got.Kind != tc.want.Kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.want.Kind)
			}
			if got.ReadyProbe != tc.want.ReadyProbe {
				t.Errorf("ReadyProbe = %+v, want %+v", got.ReadyProbe, tc.want.ReadyProbe)
			}
			if len(got.CmdTemplate) == 0 {
				t.Errorf("CmdTemplate is empty; want non-empty argv with one %%d")
			}
		})
	}
}

// TestExpoCmdTemplateBootstrap pins the self-healing bootstrap logic in
// expoCmdTemplate so a future edit can't silently drop the clean-reinstall
// guard, the completion marker, or — critically — move the marker back
// inside the user's repo.
func TestExpoCmdTemplateBootstrap(t *testing.T) {
	t.Parallel()
	// The template is a sh -c invocation; the shell command is in index 2.
	if len(expoCmdTemplate) < 3 {
		t.Fatalf("expoCmdTemplate len = %d, want ≥ 3 (sh -c <cmd>)", len(expoCmdTemplate))
	}
	cmd := expoCmdTemplate[2]
	for _, c := range []struct{ want, desc string }{
		{"../.clank-preview-bootstrap/", "completion marker in the host work-root (not the repo)"},
		{`.bun"`, "installer-specific marker suffix (forces clean reinstall on installer change)"},
		{"rm -rf node_modules", "clean-reinstall on missing marker"},
		{"bun install", "install step present"},
		{"--no-save", "install must not touch package.json in the user's repo"},
		{`rm -f bun.lock`, "migrated-lockfile cleanup (bun writes bun.lock from package-lock.json even under --no-save)"},
		{"keep_lock", "pre-existing bun.lock guard — genuinely-bun repos keep their lockfile"},
		{"expo start", "metro start present"},
	} {
		if !strings.Contains(cmd, c.want) {
			t.Errorf("expoCmdTemplate missing %s: %q not found in shell command", c.desc, c.want)
		}
	}
	// The marker must never live inside the user's repo (node_modules or any
	// in-tree path) — keeping the repo clean is the whole point of the
	// work-root location; guard against a regression.
	if strings.Contains(cmd, "node_modules/.clank") {
		t.Errorf("bootstrap marker must not live inside node_modules: %q", cmd)
	}
}

func TestDetectIOError(t *testing.T) {
	t.Parallel()
	// Pass a path that exists but isn't a directory; ReadFile returns
	// EISDIR/ENOTDIR. Detect should bubble it, not silently return nil.
	dir := t.TempDir()
	// package.json is a directory — ReadFile will fail.
	if err := os.Mkdir(filepath.Join(dir, "package.json"), 0o755); err != nil {
		t.Fatalf("mkdir trap: %v", err)
	}
	_, err := Detect(dir)
	if err == nil {
		t.Fatalf("Detect: want error for unreadable package.json, got nil")
	}
}
