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

// TestDetect_DefaultInstallsWithBun pins the no-signal default: with
// no lockfile or packageManager field (clank-created template
// projects), bun installs — and the LAUNCH goes through the
// worktree-local node_modules/.bin regardless, never npx (which would
// add a registry fetch, a network dependency, and a prompt) and never
// a manager-specific runner: install and launch are resolved
// independently.
func TestDetect_DefaultInstallsWithBun(t *testing.T) {
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
		if !strings.Contains(cmd, "bun install --no-save") {
			t.Errorf("%s recipe must install with bun's reconciling default (no lockfile to freeze against): %q", name, cmd)
		}
		if !strings.Contains(cmd, "exec node_modules/.bin/") {
			t.Errorf("%s recipe must launch via node_modules/.bin: %q", name, cmd)
		}
		if strings.Contains(cmd, "exec bun ") || strings.Contains(cmd, "npx") {
			t.Errorf("%s recipe launched via a manager instead of the layout: %q", name, cmd)
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
// with its own lockfile is installed by its own package manager in
// FROZEN mode — the lockfile is reproduced exactly and drift fails
// loudly instead of dirtying the user's checkout — and the launch
// derives from the layout (.bin, or yarn for yarn repos), not the
// installer.
func TestDetect_ReusesProjectPackager(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		lockfile    string
		wantInstall string
		wantExec    string
	}{
		{"pnpm", "pnpm-lock.yaml", "pnpm install --frozen-lockfile", "exec node_modules/.bin/vite"},
		{"npm", "package-lock.json", "npm ci", "exec node_modules/.bin/vite"},
		{"yarn", "yarn.lock", "yarn install --frozen-lockfile", "exec yarn vite"},
		{"bun", "bun.lock", "bun install --frozen-lockfile", "exec node_modules/.bin/vite"},
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
			if tc.name != string(PackagerBun) && strings.Contains(cmd, "bun install") {
				t.Errorf("recipe must not fall back to bun for a %s project: %q", tc.name, cmd)
			}
			// The shared-checkout posture: an existing tree is never
			// installed over.
			if !strings.Contains(cmd, "[ -d node_modules ]") {
				t.Errorf("local recipe missing the deps-present skip: %q", cmd)
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

// TestDetect_PnPLayoutLaunchesViaYarn pins the layout half of the
// install/launch split: committed PnP artifacts route the launch
// through `yarn <tool>` (there is no node_modules/.bin to exec) even
// when the declared-manager walk says something else entirely.
func TestDetect_PnPLayoutLaunchesViaYarn(t *testing.T) {
	t.Parallel()
	spec := detectSpec(t, map[string]string{
		"package.json":      `{"devDependencies":{"vite":"^6.0.0"}}`,
		"package-lock.json": "{}",
		".pnp.cjs":          "// zero-install artifact",
	})
	if cmd := shellOf(t, spec); !strings.Contains(cmd, "exec yarn vite") {
		t.Errorf("PnP layout must launch via yarn: %q", cmd)
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
	if !strings.Contains(cmd, "bun install --no-save") || !strings.Contains(cmd, "exec node_modules/.bin/vite") {
		t.Errorf("always-bun recipe must install via bun and launch via .bin: %q", cmd)
	}
	if strings.Contains(cmd, "pnpm") {
		t.Errorf("always-bun recipe consulted the lockfile for the install: %q", cmd)
	}
	// Cloud machines own their trees: the install runs unconditionally,
	// with no deps-present skip.
	if strings.Contains(cmd, "[ -d node_modules ]") {
		t.Errorf("always-bun recipe must not skip installs on present deps: %q", cmd)
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

// clank.yaml handling: explicit config wins over detection and
// policy alike.
func TestDetect_ClankYAML(t *testing.T) {
	t.Parallel()

	t.Run("custom command bypasses detection", func(t *testing.T) {
		t.Parallel()
		spec := detectSpec(t, map[string]string{
			"clank.yaml": "preview:\n  command: ./serve.sh --listen ${PORT}\n",
		})
		if spec.Kind != KindWeb {
			t.Errorf("Kind = %q, want %q (custom commands are the browser flow)", spec.Kind, KindWeb)
		}
		if cmd := shellOf(t, spec); cmd != "./serve.sh --listen ${PORT}" {
			t.Errorf("command not verbatim: %q", cmd)
		}
		if spec.ReadyProbe != webReadyProbe {
			t.Errorf("ReadyProbe = %+v, want default %+v", spec.ReadyProbe, webReadyProbe)
		}
		if spec.Installer != "" || spec.RequiredTool != "" {
			t.Errorf("custom command without install must not carry Installer/RequiredTool: %q/%q", spec.Installer, spec.RequiredTool)
		}
	})

	t.Run("custom command wins over a detectable framework", func(t *testing.T) {
		t.Parallel()
		spec := detectSpec(t, map[string]string{
			"package.json": `{"devDependencies":{"vite":"^6.0.0"}}`,
			"clank.yaml":   "preview:\n  command: ./serve.sh --listen ${PORT}\n",
		})
		if cmd := shellOf(t, spec); strings.Contains(cmd, "vite") {
			t.Errorf("detection leaked into a custom command: %q", cmd)
		}
	})

	t.Run("ready override", func(t *testing.T) {
		t.Parallel()
		spec := detectSpec(t, map[string]string{
			"clank.yaml": "preview:\n  command: ./serve.sh ${PORT}\n  ready:\n    path: /healthz\n    expect: ok\n",
		})
		want := ReadyProbe{Path: "/healthz", ExpectedSubstr: "ok"}
		if spec.ReadyProbe != want {
			t.Errorf("ReadyProbe = %+v, want %+v", spec.ReadyProbe, want)
		}
	})

	t.Run("custom command with install gets the bootstrap", func(t *testing.T) {
		t.Parallel()
		spec := detectSpec(t, map[string]string{
			"clank.yaml": "preview:\n  install: make deps\n  command: make serve PORT=${PORT}\n",
		})
		cmd := shellOf(t, spec)
		if !strings.Contains(cmd, "make deps") || !strings.Contains(cmd, "make serve PORT=${PORT}") {
			t.Errorf("bootstrap missing install or command: %q", cmd)
		}
		if strings.Contains(cmd, "exec make serve") {
			t.Errorf("user commands must not be exec-prefixed (compound commands would break): %q", cmd)
		}
		if spec.Installer != "make deps" {
			t.Errorf("Installer = %q, want the verbatim install command", spec.Installer)
		}
	})

	t.Run("install override on a detected framework", func(t *testing.T) {
		t.Parallel()
		spec := detectSpec(t, map[string]string{
			"package.json":      `{"devDependencies":{"vite":"^6.0.0"}}`,
			"package-lock.json": "{}",
			"clank.yaml":        "preview:\n  install: npm ci\n",
		})
		cmd := shellOf(t, spec)
		if !strings.Contains(cmd, "npm ci") || strings.Contains(cmd, "npm install") {
			t.Errorf("install override not applied: %q", cmd)
		}
		if !strings.Contains(cmd, "exec node_modules/.bin/vite") {
			t.Errorf("detected launch must survive an install override: %q", cmd)
		}
		if spec.Installer != "npm ci" {
			t.Errorf("Installer = %q, want the override", spec.Installer)
		}
		// npm only mattered for the install the override replaced.
		if spec.RequiredTool != "" {
			t.Errorf("RequiredTool = %q, want none with the install overridden", spec.RequiredTool)
		}
	})

	t.Run("install override beats the always-bun policy", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{
			".git/":        "",
			"package.json": `{"devDependencies":{"vite":"^6.0.0"}}`,
			"clank.yaml":   "preview:\n  install: pnpm install --prod=false\n",
		})
		spec, err := Detect(dir, PackagerPolicyAlwaysBun)
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if !strings.Contains(shellOf(t, spec), "pnpm install --prod=false") {
			t.Errorf("explicit preview.install must win in the cloud too: %q", shellOf(t, spec))
		}
	})

	t.Run("dir re-roots detection", func(t *testing.T) {
		t.Parallel()
		spec := detectSpec(t, map[string]string{
			"clank.yaml":           "preview:\n  dir: web-app\n",
			"web-app/package.json": `{"devDependencies":{"vite":"^6.0.0"}}`,
		})
		if spec.Kind != KindWeb || spec.Dir != "web-app" {
			t.Errorf("Kind/Dir = %q/%q, want web/web-app", spec.Kind, spec.Dir)
		}
	})

	t.Run("dir applies to custom commands", func(t *testing.T) {
		t.Parallel()
		spec := detectSpec(t, map[string]string{
			"clank.yaml": "preview:\n  dir: site\n  command: ./serve.sh ${PORT}\n",
			"site/":      "",
		})
		if spec.Dir != "site" {
			t.Errorf("Dir = %q, want site", spec.Dir)
		}
	})

	t.Run("missing dir errors", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{
			".git/":      "",
			"clank.yaml": "preview:\n  dir: gone\n",
		})
		_, err := Detect(dir, PackagerPolicyReuseProject)
		if err == nil || !strings.Contains(err.Error(), "preview.dir") {
			t.Fatalf("err = %v, want preview.dir error", err)
		}
	})

	t.Run("dir escaping via symlink errors", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		outside := t.TempDir()
		writeTree(t, outside, map[string]string{
			"package.json": `{"dependencies":{"vite":"^6.0.0"}}`,
		})
		writeTree(t, dir, map[string]string{
			".git/":      "",
			"clank.yaml": "preview:\n  dir: web-app\n",
		})
		if err := os.Symlink(outside, filepath.Join(dir, "web-app")); err != nil {
			t.Fatal(err)
		}
		_, err := Detect(dir, PackagerPolicyReuseProject)
		if err == nil || !strings.Contains(err.Error(), "preview.dir") {
			t.Fatalf("err = %v, want preview.dir escape error", err)
		}
	})

	t.Run("configured preview with nothing to run errors", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{
			".git/":      "",
			"clank.yaml": "preview:\n  install: npm install\n",
		})
		_, err := Detect(dir, PackagerPolicyReuseProject)
		if err == nil || !strings.Contains(err.Error(), "no supported framework") {
			t.Fatalf("err = %v, want configured-but-nothing-to-run error", err)
		}
	})

	t.Run("empty preview section stays not-previewable", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{
			".git/":      "",
			"clank.yaml": "preview: {}\n",
		})
		spec, err := Detect(dir, PackagerPolicyReuseProject)
		if err != nil || spec != nil {
			t.Fatalf("Detect = %+v, %v; want nil, nil", spec, err)
		}
	})

	t.Run("invalid clank.yaml errors loudly", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{
			".git/":        "",
			"package.json": `{"devDependencies":{"vite":"^6.0.0"}}`,
			"clank.yaml":   "preview:\n  command: no placeholder here\n",
		})
		_, err := Detect(dir, PackagerPolicyReuseProject)
		if err == nil {
			t.Fatalf("Detect: want error for invalid clank.yaml on an otherwise-previewable dir, got nil")
		}
	})
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
