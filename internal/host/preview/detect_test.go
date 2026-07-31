package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// detectSpec runs Detect (reuse-project policy) over a fixture tree (a
// .git dir is always planted so ResolvePackager's walk-up stays inside
// the fixture) and fails the test on error or a nil spec.
func detectSpec(t *testing.T, files map[string]string) *Spec {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, files)
	writeTree(t, dir, map[string]string{".git/": ""})
	spec, err := Detect(dir, PackagerPolicyReuseProject)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if spec == nil {
		t.Fatalf("Detect: want spec for fixture %v, got nil", files)
	}
	return spec
}

// shellOf returns the sh -c body of a spec's CmdTemplate.
func shellOf(t *testing.T, spec *Spec) string {
	t.Helper()
	if len(spec.CmdTemplate) != 3 || spec.CmdTemplate[0] != "sh" || spec.CmdTemplate[1] != "-c" {
		t.Fatalf("CmdTemplate = %v, want [sh -c <cmd>]", spec.CmdTemplate)
	}
	return spec.CmdTemplate[2]
}

// Detect's job is small: classify a directory as previewable or not.
// The table doubles as the spec for what we mean by "Expo project" /
// "web project" — adding a new positive or negative row IS the place
// to document an edge case.
func TestDetect(t *testing.T) {
	t.Parallel()

	type fixture struct {
		// name is the subtest name.
		name string
		// files maps "relative/path" → file contents to create under
		// the test's temp dir.
		files map[string]string
		// want is what Detect should return: nil for not-previewable,
		// &Spec{...} for previewable. We compare the spec's Kind and
		// ReadyProbe; CmdTemplate contents are pinned separately.
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
			name: "next via dependencies",
			files: map[string]string{
				"package.json": `{"dependencies":{"next":"^15.0.0","react":"^19.0.0"}}`,
			},
			want: &Spec{Kind: KindWeb, ReadyProbe: webReadyProbe},
		},
		{
			name: "next wins over vite when both are declared",
			files: map[string]string{
				"package.json": `{"dependencies":{"next":"^15.0.0"},"devDependencies":{"vite":"^6.0.0"}}`,
			},
			want: &Spec{Kind: KindWeb, ReadyProbe: webReadyProbe},
		},
		{
			name: "vite via devDependencies",
			files: map[string]string{
				"package.json": `{"devDependencies":{"vite":"^6.0.0","svelte":"^5.0.0"}}`,
			},
			want: &Spec{Kind: KindWeb, ReadyProbe: webReadyProbe},
		},
		{
			name: "vite via dependencies",
			files: map[string]string{
				"package.json": `{"dependencies":{"vite":"^6.0.0"}}`,
			},
			want: &Spec{Kind: KindWeb, ReadyProbe: webReadyProbe},
		},
		{
			name: "expo wins over vite when both are declared",
			files: map[string]string{
				"package.json": `{"dependencies":{"expo":"~50.0.0"},"devDependencies":{"vite":"^6.0.0"}}`,
				"app.json":     `{"expo":{"name":"x"}}`,
			},
			want: &Spec{Kind: KindExpo, ReadyProbe: expoReadyProbe},
		},
		{
			name: "expo dep without app config falls through to vite",
			files: map[string]string{
				"package.json": `{"dependencies":{"expo":"~50.0.0"},"devDependencies":{"vite":"^6.0.0"}}`,
			},
			want: &Spec{Kind: KindWeb, ReadyProbe: webReadyProbe},
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
			writeTree(t, dir, tc.files)
			writeTree(t, dir, map[string]string{".git/": ""})

			got, err := Detect(dir, PackagerPolicyReuseProject)
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
				t.Errorf("CmdTemplate is empty; want non-empty argv with a port placeholder")
			}
		})
	}
}

// TestDetect_MonorepoSubdir pins that Detect is directory-precise: a
// monorepo root without a dev-server dep is not previewable while its
// web-app/ subdir is — the contract subdir preview start relies on
// (PreviewStartLocal hands Detect the requested subdir, not the root).
func TestDetect_MonorepoSubdir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/":                "",
		"package.json":         `{"dependencies":{"react":"^18"}}`,
		"web-app/package.json": `{"devDependencies":{"vite":"^6.0.0"}}`,
	})

	rootSpec, err := Detect(root, PackagerPolicyReuseProject)
	if err != nil {
		t.Fatalf("Detect(root): %v", err)
	}
	if rootSpec != nil {
		t.Errorf("Detect(root) = %+v, want nil (react-only root is not previewable)", rootSpec)
	}
	subSpec, err := Detect(filepath.Join(root, "web-app"), PackagerPolicyReuseProject)
	if err != nil {
		t.Fatalf("Detect(web-app): %v", err)
	}
	if subSpec == nil || subSpec.Kind != KindWeb {
		t.Errorf("Detect(web-app) = %+v, want KindWeb", subSpec)
	}
}

// TestDetect_DefaultLaunchesViaBun pins the no-signal default: with no
// lockfile or packageManager field (clank-created template projects),
// every recipe execs the worktree-local bin via bun instead of npx —
// detection guarantees the dep is declared and the bootstrap's install
// materializes its bin, so an npx registry fetch at spawn time would
// only add latency, a network dependency, and a prompt to suppress.
func TestDetect_DefaultLaunchesViaBun(t *testing.T) {
	t.Parallel()
	for name, files := range map[string]map[string]string{
		"expo": {
			"package.json": `{"dependencies":{"expo":"~50.0.0"}}`,
			"app.json":     `{"expo":{}}`,
		},
		"vite": {"package.json": `{"devDependencies":{"vite":"^6.0.0"}}`},
		"next": {"package.json": `{"dependencies":{"next":"^15.0.0"}}`},
	} {
		spec := detectSpec(t, files)
		cmd := shellOf(t, spec)
		if !strings.Contains(cmd, "exec bun ") {
			t.Errorf("%s recipe must exec the dev server via bun: %q", name, cmd)
		}
		if strings.Contains(cmd, "npx") {
			t.Errorf("%s recipe must not invoke npx: %q", name, cmd)
		}
		if spec.Installer != string(PackagerBun) {
			t.Errorf("%s Installer = %q, want %q", name, spec.Installer, PackagerBun)
		}
		if spec.RequiredTool != string(PackagerBun) || spec.ToolEvidence != "" {
			t.Errorf("%s RequiredTool/ToolEvidence = %q/%q, want bun with no evidence", name, spec.RequiredTool, spec.ToolEvidence)
		}
	}
}

// TestDetect_ReusesProjectPackager is the heart of the
// stop-imposing-bun change: under the reuse-project policy, a project
// with its own lockfile is installed by its own package manager
// against that lockfile, and the dev tool is launched through the
// manager-appropriate form.
func TestDetect_ReusesProjectPackager(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		lockfile    string
		wantInstall string
		wantExec    string
	}{
		{"pnpm", "pnpm-lock.yaml", "pnpm install", "exec node_modules/.bin/vite"},
		{"npm", "package-lock.json", "npm install", "exec node_modules/.bin/vite"},
		{"yarn", "yarn.lock", "yarn install", "exec yarn vite"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := detectSpec(t, map[string]string{
				"package.json": `{"devDependencies":{"vite":"^6.0.0"}}`,
				tc.lockfile:    "",
			})
			cmd := shellOf(t, spec)
			if !strings.Contains(cmd, tc.wantInstall) {
				t.Errorf("recipe missing %q: %q", tc.wantInstall, cmd)
			}
			if !strings.Contains(cmd, tc.wantExec) {
				t.Errorf("recipe missing %q: %q", tc.wantExec, cmd)
			}
			if strings.Contains(cmd, "bun install") {
				t.Errorf("recipe must not fall back to bun for a %s project: %q", tc.name, cmd)
			}
			if spec.Installer != tc.name {
				t.Errorf("Installer = %q, want %q", spec.Installer, tc.name)
			}
			if spec.RequiredTool != tc.name || !strings.Contains(spec.ToolEvidence, tc.lockfile) {
				t.Errorf("RequiredTool/ToolEvidence = %q/%q, want %s via %s", spec.RequiredTool, spec.ToolEvidence, tc.name, tc.lockfile)
			}
		})
	}
}

// TestDetect_PolicyAlwaysBun pins the cloud posture: the always-bun
// policy installs with bun REGARDLESS of the repo's own lockfile —
// machine worktrees are materialized fresh (no user tree to protect)
// and bun's speed is the deliberate trade. The paired reuse-project
// assertion above (TestDetect_ReusesProjectPackager) is the laptop
// side; together they document the split.
func TestDetect_PolicyAlwaysBun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".git/":          "",
		"package.json":   `{"devDependencies":{"vite":"^6.0.0"}}`,
		"pnpm-lock.yaml": "",
	})
	spec, err := Detect(dir, PackagerPolicyAlwaysBun)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	cmd := shellOf(t, spec)
	if !strings.Contains(cmd, "bun install --no-save") || !strings.Contains(cmd, "exec bun vite") {
		t.Errorf("always-bun recipe must install and launch via bun: %q", cmd)
	}
	if strings.Contains(cmd, "pnpm") {
		t.Errorf("always-bun recipe consulted the lockfile: %q", cmd)
	}
	if spec.Installer != string(PackagerBun) || spec.RequiredTool != string(PackagerBun) {
		t.Errorf("Installer/RequiredTool = %q/%q, want bun/bun", spec.Installer, spec.RequiredTool)
	}
}

// TestDetect_UnknownPolicyErrors pins the no-fallbacks contract: a
// typo'd policy is a wiring bug and must not silently pick a side.
func TestDetect_UnknownPolicyErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".git/":        "",
		"package.json": `{"devDependencies":{"vite":"^6.0.0"}}`,
	})
	if _, err := Detect(dir, PackagerPolicy("bun-sometimes")); err == nil {
		t.Fatal("Detect accepted an unknown policy")
	}
}

// TestDetect_SavedChoiceWins pins the one-time prompt's contract: a
// saved per-project choice beats lockfile detection under the
// reuse-project policy (and is ignored under always-bun, which never
// consults the repo side at all). Not parallel: CLANK_DIR pins the
// choice location.
func TestDetect_SavedChoiceWins(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".git/":             "",
		"package.json":      `{"devDependencies":{"vite":"^6.0.0"}}`,
		"package-lock.json": "{}",
	})
	if err := SavePackagerChoice(dir, PackagerBun); err != nil {
		t.Fatalf("SavePackagerChoice: %v", err)
	}
	spec, err := Detect(dir, PackagerPolicyReuseProject)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if spec.Installer != string(PackagerBun) {
		t.Errorf("Installer = %q, want the saved bun choice to beat package-lock.json", spec.Installer)
	}
	if spec.ToolEvidence != packagerChoiceEvidence {
		t.Errorf("ToolEvidence = %q, want %q", spec.ToolEvidence, packagerChoiceEvidence)
	}
}

func TestPackagerChoiceRoundTrip(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	dir := t.TempDir()

	if _, ok := LoadPackagerChoice(dir); ok {
		t.Fatal("LoadPackagerChoice: want no choice before save")
	}
	if err := SavePackagerChoice(dir, PackagerPNPM); err != nil {
		t.Fatalf("SavePackagerChoice: %v", err)
	}
	pm, ok := LoadPackagerChoice(dir)
	if !ok || pm != PackagerPNPM {
		t.Fatalf("LoadPackagerChoice = %q, %v; want pnpm, true", pm, ok)
	}

	// A corrupt file re-prompts instead of driving an undefined install.
	path, err := packagerChoicePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("weirdpm"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadPackagerChoice(dir); ok {
		t.Fatal("LoadPackagerChoice honored a corrupt choice file")
	}

	if err := SavePackagerChoice(dir, Packager("weirdpm")); err == nil {
		t.Fatal("SavePackagerChoice accepted an unknown packager")
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
	_, err := Detect(dir, PackagerPolicyReuseProject)
	if err == nil {
		t.Fatalf("Detect: want error for unreadable package.json, got nil")
	}
}
