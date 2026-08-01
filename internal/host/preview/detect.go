package preview

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/config"
)

// expoCmdTemplate is the argv that spawns Metro for a detected Expo
// project ("%d" is the allocated port, substituted by renderArgs).
//
// We wrap the invocation in `sh -c` to run `bun install` first: a
// materialized worktree only carries what's tracked in git, and
// node_modules is gitignored, so the first /preview/start on a fresh
// worktree must install before Metro can start. The install is a fast
// no-op once node_modules is already present.
//
// bun installs and launches, but Metro still runs under Node: `bun expo`
// executes the worktree-local expo bin (which `bun install` just
// materialized — no registry fetch at spawn time), and bun respects its
// `#!/usr/bin/env node` shebang. That matters: the preview shim rides
// NODE_OPTIONS (see spawn.buildEnv), which only Node honors.
// bun over npm because it hard-links node_modules from its global cache:
// sibling worktrees of the same repo cost links instead of gigabytes of
// small-file writes, which is what an I/O-constrained sprite needs (the
// cache lives under $HOME, same filesystem as the worktrees; a
// cross-device cache would silently degrade to copies). `--no-save` keeps
// bun from touching package.json — but when the repo has a
// package-lock.json, bun migrates it and writes a bun.lock EVEN under
// --no-save (verified on bun 1.3.11/1.3.14; no flag disables it), so the
// bootstrap deletes the migrated bun.lock afterward unless one existed
// before the install (a genuinely-bun repo keeps its own). The user's repo
// stays exactly as materialized. bun also refuses to run dependency
// postinstall scripts outside its trusted allowlist, which we want:
// preview installs run unattended on the user's behalf.
//
// Self-healing bootstrap. A completion marker records "this workdir's
// node_modules was installed by bun". It lives in the clank state dir
// (<config.Dir()>/preview-bootstrap/, see bootstrapMarkerPath) — never
// inside or next to the user's repo — and spawn injects its path via
// $CLANK_PREVIEW_BOOTSTRAP_MARKER so the shell does no path assembly.
// It's written only AFTER a successful install, so an interrupted prior
// run (marker absent) forces a clean reinstall rather than trusting a
// half-extracted tree. The installer-specific `.bun` suffix makes
// workdirs installed by the old npm bootstrap (or a future different
// installer) reinstall cleanly once instead of running one package
// manager over another's tree.
//
// Install output is deliberately not silenced: it streams to the client
// (ring buffer → /preview/logs) so the multi-minute first-run install
// shows live progress instead of a blind spinner.
//
// `exec` replaces the shell with bun (which then reaches Metro via its
// node shebang) so signals + Setpgid target the whole tree directly.
// `--non-interactive` tells Expo CLI to skip prompts; we
// deliberately do NOT set CI=true (Metro reads CI and disables watch mode
// + HMR). EXPO_NO_DOTENV is set in spawn.buildEnv. (V1 bootstrap; the
// long-term shape is a per-repo clank.yaml bootstrap step — see doc.go.)
//
// Raw string (backticks) so the embedded shell double-quotes don't need
// escaping.
var expoCmdTemplate = bootstrapTemplate(`bun expo start --port %d --non-interactive`)

// bootstrapMarkerEnv carries the bootstrap completion-marker path from
// spawn into the bootstrapTemplate shell. Env rather than in-shell path
// assembly: the Go side owns the location (config dir, hash key) and
// the value needs no shell quoting.
const bootstrapMarkerEnv = "CLANK_PREVIEW_BOOTSTRAP_MARKER"

// bootstrapMarkerPath returns the completion-marker file for workDir's
// bun bootstrap. Lives under the clank state dir so nothing is written
// inside — or next to — the user's repo; keyed by the workdir's
// basename plus a hash of its absolute path so same-named folders
// (a monorepo's web-app/, sibling clones) can't collide.
func bootstrapMarkerPath(workDir string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", fmt.Errorf("resolve clank dir: %w", err)
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve workdir %q: %w", workDir, err)
	}
	sum := sha256.Sum256([]byte(abs))
	name := fmt.Sprintf("%s-%x.bun", filepath.Base(abs), sum[:6])
	return filepath.Join(dir, "preview-bootstrap", name), nil
}

// bootstrapTemplate wraps a dev-server exec line in the self-healing
// bun-install bootstrap documented on expoCmdTemplate. The returned argv
// is a Spec.CmdTemplate; a "%d" placeholder inside execLine survives
// verbatim for renderArgs to substitute. Fails fast when the marker env
// is missing — a caller bug, never a reason to guess a location.
func bootstrapTemplate(execLine string) []string {
	return []string{
		"sh", "-c",
		`m="$` + bootstrapMarkerEnv + `"; ` +
			`[ -n "$m" ] || { echo "` + bootstrapMarkerEnv + ` is not set" >&2; exit 1; }; ` +
			`[ -f "$m" ] || rm -rf node_modules; ` +
			`keep_lock=; [ -f bun.lock ] && keep_lock=1; ` +
			`bun install --no-save; err=$?; ` +
			`[ -n "$keep_lock" ] || rm -f bun.lock; ` +
			`[ "$err" -eq 0 ] && ` +
			`mkdir -p "$(dirname "$m")" && : > "$m" && ` +
			`exec ` + execLine,
	}
}

// expoReadyProbe asks Metro's /status endpoint, which has returned
// "packager-status:running" stably since at least Expo SDK 49. The
// probe is the source of truth — we used to scan stdout for "Waiting
// on", which raced (Metro prints the line before serve_forever fully
// starts accepting) and shifted across SDK versions ("Metro waiting
// on" → "Waiting on").
var expoReadyProbe = ReadyProbe{
	Path:           "/status",
	ExpectedSubstr: "packager-status:running",
}

// appConfigCandidates is the set of file names a project may use to
// declare its Expo config. Any one of them, in addition to `expo` being
// a (dev)dependency, marks the worktree as previewable.
var appConfigCandidates = []string{
	"app.json",
	"app.config.js",
	"app.config.ts",
}

// Detect inspects workDir and returns a Spec if it looks like an Expo app.
// Web launch behavior is intentionally not inferred here; it comes from the
// strict launch configuration generated during one-time setup. The contract:
//
//   - (nil, nil) means "not previewable" — a normal answer, NOT an
//     error. Surface it as preview_available: false / available: false
//     to the client.
//   - (nil, err) means I/O blew up reading the worktree. The caller
//     should log and treat as not-previewable, but a flood of these
//     signals a real problem (disk, perms, racy worktree removal).
//   - (*Spec, nil) means the dev server should be spawnable with the
//     returned recipe.
//
// Detection is intentionally cheap (one Stat for
// package.json, one small JSON parse, up to three more Stats for
// app.config files) so callers can run it per worktree-list row
// without caching.
func Detect(workDir string) (*Spec, error) {
	pkgPath := filepath.Join(workDir, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", pkgPath, err)
	}

	if packageHasExpo(data) && hasExpoAppConfig(workDir) {
		return &Spec{
			Kind:                 KindExpo,
			CmdTemplate:          append([]string(nil), expoCmdTemplate...),
			ShouldSubstitutePort: true,
			ReadyProbe:           expoReadyProbe,
		}, nil
	}

	return nil, nil
}

// packageHasExpo reports whether the parsed package.json declares
// `expo` in either dependencies or devDependencies. We accept both —
// some templates put Expo CLI in devDependencies while still being a
// genuine Expo app. The cost of a false positive (Detect returns a
// Spec for a non-Expo project) is "the Preview button shows but the
// spawn fails on a missing binary" — recoverable and visible.
func packageHasExpo(data []byte) bool {
	return packageHasDep(data, "expo")
}

// packageHasDep reports whether the parsed package.json declares name
// in either dependencies or devDependencies (Expo templates use both).
//
// Decoding into typed maps instead of map[string]any avoids the
// any-walk for the common case where neither key exists.
func packageHasDep(data []byte, name string) bool {
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		// Malformed package.json: treat as "not previewable" rather
		// than bubble the error. A real app with a broken package.json
		// wouldn't run its dev server either.
		return false
	}
	if _, ok := pkg.Dependencies[name]; ok {
		return true
	}
	if _, ok := pkg.DevDependencies[name]; ok {
		return true
	}
	return false
}

// hasExpoAppConfig reports whether the worktree has at least one of the
// app-config files Expo recognizes. Stat-based — no file content read.
func hasExpoAppConfig(workDir string) bool {
	for _, name := range appConfigCandidates {
		if _, err := os.Stat(filepath.Join(workDir, name)); err == nil {
			return true
		}
	}
	return false
}
