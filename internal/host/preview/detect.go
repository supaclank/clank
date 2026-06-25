package preview

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// expoCmdTemplate is the argv used to spawn Metro for a detected Expo
// project. "%d" is the allocated port.
//
// We wrap the actual expo invocation in `sh -c` so we can run an
// idempotent `npm install` first. A materialized worktree only carries
// what's tracked in git — node_modules is gitignored, so the first
// /preview/start on a fresh worktree would otherwise crash with
// "expo CLI not found" or hang waiting on npx's auto-install prompt.
// `npm install` is a no-op on the second + subsequent runs (it just
// re-verifies the lockfile), so the overhead is bounded.
//
// `--silent --no-audit --no-fund` keeps stdout clean so the ringbuf
// logs don't drown in npm chatter; `exec` after the install replaces
// the shell process so signals + Setpgid still target Metro directly.
//
// `npx --yes` skips the "Ok to proceed?" prompt that npx shows when
// it needs to install something globally — defensive, since we just
// ran npm install locally, but cheap insurance against version skew.
//
// `--non-interactive` tells Expo CLI to skip every prompt — package-
// update offers, "do you want to install <peer dep>?" boxes,
// telemetry consent, etc. We deliberately do NOT set CI=true in the
// process env: Metro reads CI and disables watch mode + HMR ("Metro
// is running in CI mode, reloads are disabled"). Targeted CLI flag
// beats a sledgehammer env var.
//
// EXPO_NO_DOTENV (set in spawn.buildEnv) stops Metro from reading
// the repo's .env into its orchestration env; npm_config_yes (also
// there) covers npm's prompts.
//
// This is V1 of the bootstrap story. The right long-term shape is a
// per-repo clank.yaml with a declared bootstrap step + an
// agent-driven fallback when no config exists — see the future-
// direction note in doc.go.
// Self-healing bootstrap: `.clank-bootstrap-ok` (inside node_modules) is
// written only AFTER a successful install, so an interrupted prior run
// (marker absent) forces `rm -rf node_modules` + a clean reinstall — npm
// won't otherwise repair a half-extracted tree (its hidden lockfile marks
// partial packages "installed" and skips re-extracting them, which is how a
// SIGKILL'd install leaves modules permanently unresolvable). With the
// marker present, `npm install` is a fast incremental no-op. %d is the port.
var expoCmdTemplate = []string{
	"sh", "-c",
	"([ -f node_modules/.clank-bootstrap-ok ] || rm -rf node_modules) && " +
		"npm install --silent --no-audit --no-fund && " +
		"touch node_modules/.clank-bootstrap-ok && " +
		"exec npx --yes expo start --port %d --non-interactive",
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
