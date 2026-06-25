package preview

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// expoCmdTemplate is the argv that spawns Metro for a detected Expo
// project ("%d" is the allocated port, substituted by renderArgs).
//
// We wrap the invocation in `sh -c` to run `npm install` first: a
// materialized worktree only carries what's tracked in git, and
// node_modules is gitignored, so the first /preview/start on a fresh
// worktree must install before Metro can start. `npm install` is a fast
// no-op once node_modules is already present.
//
// Self-healing bootstrap. A completion marker lives in the host work-root
// — a sibling of the worktree, NEVER inside the user's repo:
// ../.clank-preview-bootstrap/<worktree-id> (the worktree is our cwd, so
// `..` is the work-root and basename(pwd) is its id). It's written only
// AFTER a successful install, so an interrupted prior run (marker absent)
// forces a clean reinstall — npm can't repair a half-extracted tree on its
// own (its hidden lockfile marks partial packages "installed" and skips
// re-extracting them, which is how a killed install leaves modules
// permanently unresolvable). `npm ci` is npm's only clean-by-construction
// alternative, but it re-extracts everything on EVERY run; the long
// readiness budget + graceful SIGTERM already make a mid-install kill rare,
// so the marker is just the cheap recovery for the residual hard-crash case.
//
// We pass `--no-audit --no-fund` (drop the audit report + funding banner)
// but deliberately NOT `--silent`: npm's install output is streamed to the
// client (ring buffer → /preview/logs) so the multi-minute first-run
// install shows live progress instead of a blind spinner.
//
// `exec` replaces the shell with Metro so signals + Setpgid target it
// directly. `npx --yes` skips npx's install prompt. `--non-interactive`
// tells Expo CLI to skip prompts; we deliberately do NOT set CI=true (Metro
// reads CI and disables watch mode + HMR). EXPO_NO_DOTENV + npm_config_yes
// are set in spawn.buildEnv. (V1 bootstrap; the long-term shape is a
// per-repo clank.yaml bootstrap step — see doc.go.)
//
// Raw string (backticks) so the embedded shell double-quotes don't need
// escaping.
var expoCmdTemplate = []string{
	"sh", "-c",
	`m="../.clank-preview-bootstrap/$(basename "$(pwd)")"; ` +
		`[ -f "$m" ] || rm -rf node_modules; ` +
		`npm install --no-audit --no-fund && ` +
		`mkdir -p "$(dirname "$m")" && : > "$m" && ` +
		`exec npx --yes expo start --port %d --non-interactive`,
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

// Detect inspects workDir and returns a Spec if it looks like an Expo
// app. The contract:
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
// Detection is intentionally cheap (one Stat for package.json, one
// small JSON parse, up to three more Stats for app.config files) so
// callers can run it per worktree-list row without caching.
func Detect(workDir string) (*Spec, error) {
	pkgPath := filepath.Join(workDir, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", pkgPath, err)
	}

	if !packageHasExpo(data) {
		return nil, nil
	}

	if !hasExpoAppConfig(workDir) {
		return nil, nil
	}

	return &Spec{
		Kind:        KindExpo,
		CmdTemplate: append([]string(nil), expoCmdTemplate...),
		ReadyProbe:  expoReadyProbe,
	}, nil
}

// packageHasExpo reports whether the parsed package.json declares
// `expo` in either dependencies or devDependencies. We accept both —
// some templates put Expo CLI in devDependencies while still being a
// genuine Expo app. The cost of a false positive (Detect returns a
// Spec for a non-Expo project) is "the Preview button shows but the
// spawn fails on a missing binary" — recoverable and visible.
//
// Decoding into typed maps instead of map[string]any avoids the
// any-walk for the common case where neither key exists.
func packageHasExpo(data []byte) bool {
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		// Malformed package.json: treat as "not previewable" rather
		// than bubble the error. A real Expo app with a broken
		// package.json wouldn't run `npx expo start` either.
		return false
	}
	if _, ok := pkg.Dependencies["expo"]; ok {
		return true
	}
	if _, ok := pkg.DevDependencies["expo"]; ok {
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
