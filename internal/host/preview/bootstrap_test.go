package preview

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootstrapShell_Expo pins the self-healing bootstrap in the
// synthesized Expo recipe so a future edit can't silently drop the
// marker protocol, the gated wipe, or — critically — move the marker
// back inside (or next to) the user's repo by assembling a path
// in-shell.
func TestBootstrapShell_Expo(t *testing.T) {
	t.Parallel()
	cmd := shellOf(t, detectSpec(t, map[string]string{
		"package.json": `{"dependencies":{"expo":"~50.0.0"}}`,
		"app.json":     `{"expo":{}}`,
	}))
	for _, c := range []struct{ want, desc string }{
		{`m="$` + bootstrapMarkerEnv + `"`, "completion marker path injected via env — Go owns the location"},
		{`[ -n "$m" ] ||`, "fail fast when the marker env is missing"},
		{`inst="$` + bootstrapInstallerEnv + `"`, "installer identity injected via env"},
		{`[ -z "$` + bootstrapWipeEnv + `" ] || rm -rf node_modules`, "node_modules wipe gated on Go's decision, never unconditional"},
		{`"$m` + markerInstallingSuffix + `"`, "in-flight sentinel marks clank-owned installs"},
		{"bun install", "install step present"},
		{"--no-save", "install must not touch package.json in the user's repo"},
		{`rm -f bun.lock`, "migrated-lockfile cleanup (bun writes bun.lock from package-lock.json even under --no-save)"},
		{"keep_lock", "pre-existing bun.lock guard — genuinely-bun repos keep their lockfile"},
		{"exec bun expo start", "metro launched via the worktree-local bin bun just installed"},
	} {
		if !strings.Contains(cmd, c.want) {
			t.Errorf("bootstrap missing %s: %q not found in shell command", c.desc, c.want)
		}
	}
	// The old shape wiped node_modules whenever the marker was absent —
	// which deleted a user's own node_modules on their first preview.
	// needsNodeModulesWipe owns the decision now.
	if strings.Contains(cmd, `[ -f "$m" ] ||`) {
		t.Errorf("marker absence must not trigger anything in-shell: %q", cmd)
	}
	// The marker location is Go's job (bootstrapMarkerPath); any relative
	// path assembled in-shell would land inside or next to the user's repo.
	if strings.Contains(cmd, "../.clank-preview-bootstrap") {
		t.Errorf("bootstrap marker must not be assembled relative to the workdir: %q", cmd)
	}
}

// TestBootstrapMarkerPath pins the marker location contract: under the
// clank state dir (never inside or next to the user's repo — laptop
// `clank preview` runs against the user's own folders), keyed uniquely
// per absolute workdir so same-named folders can't collide.
func TestBootstrapMarkerPath(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())

	a, err := bootstrapMarkerPath("/repos/one/web-app")
	if err != nil {
		t.Fatalf("bootstrapMarkerPath: %v", err)
	}
	b, err := bootstrapMarkerPath("/repos/two/web-app")
	if err != nil {
		t.Fatalf("bootstrapMarkerPath: %v", err)
	}
	wantDir := filepath.Join(os.Getenv("CLANK_DIR"), "preview-bootstrap")
	if filepath.Dir(a) != wantDir {
		t.Errorf("marker dir = %q, want %q", filepath.Dir(a), wantDir)
	}
	if a == b {
		t.Errorf("same-named workdirs must get distinct markers, both = %q", a)
	}
	// Deterministic: same workdir → same marker across spawns.
	a2, err := bootstrapMarkerPath("/repos/one/web-app")
	if err != nil {
		t.Fatalf("bootstrapMarkerPath: %v", err)
	}
	if a != a2 {
		t.Errorf("marker not deterministic: %q vs %q", a, a2)
	}
}

// runBootstrap executes a bootstrap shell command in workDir with the
// marker/installer envs set, plus any extra env entries.
func runBootstrap(t *testing.T, shellCmd, workDir, marker, installer string, extraEnv ...string) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", shellCmd)
	cmd.Dir = workDir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		bootstrapMarkerEnv + "=" + marker,
		bootstrapInstallerEnv + "=" + installer,
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil && testing.Verbose() {
		t.Logf("bootstrap output: %s", out)
	}
	return err
}

// TestBootstrapShell_RecordsInstallerAndClearsSentinel runs the real
// Expo recipe with a fake succeeding bun: the completion marker lands
// exactly at $CLANK_PREVIEW_BOOTSTRAP_MARKER with the installer as its
// content, the in-flight sentinel is cleaned up, and nothing is
// written inside or next to the workdir.
func TestBootstrapShell_RecordsInstallerAndClearsSentinel(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "preview-bootstrap", "worktree-abc")

	bin := t.TempDir()
	// One fake bun serves both roles: `bun install` succeeds, then the
	// `exec bun expo start` line terminates instantly.
	if err := os.WriteFile(filepath.Join(bin, "bun"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake bun: %v", err)
	}

	shellCmd := strings.ReplaceAll(shellOf(t, detectSpec(t, map[string]string{
		"package.json": `{"dependencies":{"expo":"~50.0.0"}}`,
		"app.json":     `{"expo":{}}`,
	})), "%d", "0")
	cmd := exec.Command("sh", "-c", shellCmd)
	cmd.Dir = worktree
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		bootstrapMarkerEnv + "=" + marker,
		bootstrapInstallerEnv + "=" + string(PackagerBun),
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out)
	}

	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker: want written at configured path after successful install: %v", err)
	}
	if got := strings.TrimSpace(string(content)); got != string(PackagerBun) {
		t.Errorf("marker content = %q, want %q (records WHAT installed)", got, PackagerBun)
	}
	if _, err := os.Stat(marker + markerInstallingSuffix); !os.IsNotExist(err) {
		t.Errorf("in-flight sentinel must be removed after success, stat err = %v", err)
	}
	if _, err := os.Stat(marker + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("atomic-write temp file must not survive a successful install, stat err = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "worktree" {
		t.Errorf("bootstrap wrote next to the workdir: %v", entries)
	}
}

// TestBootstrapShell_PreservesForeignNodeModules is the regression
// test for the bug that motivated the marker rework: the first
// preview of a project that already has node_modules (installed by
// the user's own package manager) must NOT delete it. The wipe runs
// only when Go's needsNodeModulesWipe says so, via bootstrapWipeEnv.
func TestBootstrapShell_PreservesForeignNodeModules(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	keep := filepath.Join(workDir, "node_modules", "left-pad", "index.js")
	writeTree(t, workDir, map[string]string{"node_modules/left-pad/index.js": "module.exports = x => x"})
	marker := filepath.Join(t.TempDir(), "m")

	shellCmd := bootstrapShell("true", "true")
	if err := runBootstrap(t, shellCmd, workDir, marker, "npm"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("first preview deleted the user's node_modules: %v", err)
	}

	// The same shell WITH the wipe env must clean the tree — Go's
	// decision is what the gate obeys.
	if err := runBootstrap(t, shellCmd, workDir, marker, "npm", bootstrapWipeEnv+"=1"); err != nil {
		t.Fatalf("bootstrap with wipe: %v", err)
	}
	if _, err := os.Stat(keep); !os.IsNotExist(err) {
		t.Errorf("wipe env set but node_modules survived, stat err = %v", err)
	}
}

// TestBootstrapShell_WipeFailureAbortsInstall pins the fix for a bug a
// review bot caught: a failed required wipe used to fall through to
// installFragment anyway, letting the new installer run over whatever
// survived the failed rm -rf — exactly the mixed-packager-artifact tree
// the wipe exists to prevent. A fake `rm` that always fails proves the
// bootstrap now aborts instead of continuing.
func TestBootstrapShell_WipeFailureAbortsInstall(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	writeTree(t, workDir, map[string]string{"node_modules/left-pad/index.js": "x"})
	marker := filepath.Join(t.TempDir(), "m")

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rm"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake rm: %v", err)
	}

	sentinel := filepath.Join(workDir, "installed")
	shellCmd := bootstrapShell("touch "+sentinel, "true")
	cmd := exec.Command("sh", "-c", shellCmd)
	cmd.Dir = workDir
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		bootstrapMarkerEnv + "=" + marker,
		bootstrapInstallerEnv + "=npm",
		bootstrapWipeEnv + "=1",
	}
	if err := cmd.Run(); err == nil {
		t.Fatalf("bootstrap: want failure when rm -rf fails, got success")
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("install ran after a failed wipe — sentinel file should not exist")
	}
}

// TestBootstrapShell_CleansMigratedLockOnInstallFailure pins the fix
// for a bug a review bot caught: bun migrates package-lock.json into
// bun.lock as a side effect BEFORE dependency resolution can fail, so
// a transient install failure (network blip, disk pressure) used to
// leave that migrated bun.lock behind. Runs the real shell snippet
// with a fake `bun` that fails after writing bun.lock, so this
// exercises actual shell semantics rather than pinning the template
// string. The failed install must also leave the in-flight sentinel
// in place — that's what routes the next spawn through a clean
// reinstall (needsNodeModulesWipe).
func TestBootstrapShell_CleansMigratedLockOnInstallFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	bin := t.TempDir()
	fakeBun := "#!/bin/sh\necho migrated > bun.lock\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "bun"), []byte(fakeBun), 0o755); err != nil {
		t.Fatalf("write fake bun: %v", err)
	}

	marker := filepath.Join(t.TempDir(), "preview-bootstrap", "worktree")
	shellCmd := strings.ReplaceAll(shellOf(t, detectSpec(t, map[string]string{
		"package.json": `{"dependencies":{"expo":"~50.0.0"}}`,
		"app.json":     `{"expo":{}}`,
	})), "%d", "0")
	cmd := exec.Command("sh", "-c", shellCmd)
	cmd.Dir = worktree
	// keep_lock=leaked simulates the uninitialized-variable half of the bug:
	// with the fix's explicit "keep_lock=;" reset, this must not survive.
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"keep_lock=leaked",
		bootstrapMarkerEnv+"="+marker,
		bootstrapInstallerEnv+"="+string(PackagerBun),
	)

	if err := cmd.Run(); err == nil {
		t.Fatalf("bootstrap: want failure (fake bun install fails), got success")
	}
	if _, err := os.Stat(filepath.Join(worktree, "bun.lock")); !os.IsNotExist(err) {
		t.Errorf("bun.lock: want removed after a failed install, stat err = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("completion marker: must not be written after a failed install")
	}
	if _, err := os.Stat(marker + markerInstallingSuffix); err != nil {
		t.Errorf("in-flight sentinel: must survive a failed install so the next spawn reinstalls cleanly: %v", err)
	}
}

func TestNeedsNodeModulesWipe(t *testing.T) {
	t.Parallel()

	const sentinelContent = "x"
	tests := []struct {
		name      string
		completed string // "" = no completion marker
		sentinel  bool
		legacyBun bool // pre-suffix-drop marker at markerPath+".bun"
		installer string
		want      bool
	}{
		{
			name:      "no markers, user tree: never wipe",
			installer: "bun",
			want:      false,
		},
		{
			name:      "legacy bun marker, same packager re-run: reuse",
			legacyBun: true,
			installer: "bun",
			want:      false,
		},
		{
			name:      "legacy bun marker, packager switch: exactly one wipe",
			legacyBun: true,
			installer: "npm",
			want:      true,
		},
		{
			name:      "interrupted first install: wipe the half-extracted tree",
			sentinel:  true,
			installer: "bun",
			want:      true,
		},
		{
			name:      "same packager re-run: reuse",
			completed: "bun",
			installer: "bun",
			want:      false,
		},
		{
			name:      "packager switch: exactly one wipe",
			completed: "npm",
			installer: "bun",
			want:      true,
		},
		{
			name:      "interrupted re-install of a completed tree heals in place",
			completed: "npm",
			sentinel:  true,
			installer: "npm",
			want:      false,
		},
		{
			name:      "interrupted re-install plus packager switch still wipes",
			completed: "npm",
			sentinel:  true,
			installer: "pnpm",
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			marker := filepath.Join(t.TempDir(), "m")
			if tt.completed != "" {
				if err := os.WriteFile(marker, []byte(tt.completed), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.sentinel {
				if err := os.WriteFile(marker+markerInstallingSuffix, []byte(sentinelContent), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.legacyBun {
				if err := os.WriteFile(marker+legacyBunMarkerSuffix, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := needsNodeModulesWipe(marker, tt.installer); got != tt.want {
				t.Errorf("needsNodeModulesWipe = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestInstallFragments_RealPackagers runs each supported package
// manager's real install fragment (through the real bootstrap shell)
// against a dependency-free fixture. Gated per manager: missing
// binaries skip with their name, per the repo's real-tool test
// pattern — no mocks.
func TestInstallFragments_RealPackagers(t *testing.T) {
	t.Parallel()
	for _, pm := range []Packager{PackagerBun, PackagerNPM, PackagerPNPM, PackagerYarn} {
		t.Run(string(pm), func(t *testing.T) {
			t.Parallel()
			if _, err := exec.LookPath(string(pm)); err != nil {
				t.Skipf("%s not on PATH", pm)
			}
			workDir := t.TempDir()
			// A name+version-only fixture: real resolution, no network.
			writeTree(t, workDir, map[string]string{
				"package.json": `{"name":"clank-fixture","version":"0.0.1","private":true}`,
			})
			marker := filepath.Join(t.TempDir(), "m")
			if err := runBootstrap(t, bootstrapShell(installFragment(pm), "true"), workDir, marker, string(pm)); err != nil {
				t.Fatalf("%s install fragment failed: %v", pm, err)
			}
			content, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("completion marker not written: %v", err)
			}
			if got := strings.TrimSpace(string(content)); got != string(pm) {
				t.Errorf("marker content = %q, want %q", got, pm)
			}
		})
	}
}
