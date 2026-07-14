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
// We wrap the invocation in `sh -c` to run `bun install` first: a
// materialized worktree only carries what's tracked in git, and
// node_modules is gitignored, so the first /preview/start on a fresh
// worktree must install before Metro can start. The install is a fast
// no-op once node_modules is already present.
//
// bun is the installer only — Metro still runs under Node via `npx expo`.
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
// Self-healing bootstrap. A completion marker lives in the host work-root
// — a sibling of the worktree, NEVER inside the user's repo:
// ../.clank-preview-bootstrap/<worktree-id>.bun (the worktree is our cwd,
// so `..` is the work-root and basename(pwd) is its id). It's written only
// AFTER a successful install, so an interrupted prior run (marker absent)
// forces a clean reinstall rather than trusting a half-extracted tree. The
// installer-specific `.bun` suffix makes worktrees installed by the old
// npm bootstrap (or a future different installer) reinstall cleanly once
// instead of running one package manager over another's tree.
//
// Install output is deliberately not silenced: it streams to the client
// (ring buffer → /preview/logs) so the multi-minute first-run install
// shows live progress instead of a blind spinner.
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
var expoCmdTemplate = bootstrapTemplate(`npx --yes expo start --port %d --non-interactive`)

// webCmdTemplate spawns Vite for a detected web project behind the same
// bun-install bootstrap as Expo (same marker file on purpose: the marker
// records "this worktree's node_modules was installed by bun", which is
// kind-independent). `--strictPort` because Manager allocated the port
// and the readiness probe polls exactly it — Vite's default
// auto-increment on a busy port would leave the probe polling a dead
// socket until timeout. `--host 127.0.0.1` because Vite's default host
// is "localhost", which Node ≥17 may resolve (and bind) as ::1 only —
// observed on macOS — while probeReady and the webpreview proxy dial
// IPv4 loopback. No --clearScreen wrangling needed: Vite only clears
// when stdout is a TTY, and ours is the ring buffer.
var webCmdTemplate = bootstrapTemplate(`npx --yes vite --port %d --strictPort --host 127.0.0.1`)

// nextCmdTemplate spawns Next.js's own dev server. `-H 127.0.0.1` for
// the same reason as Vite's --host (loopback parity with the probe and
// proxy, and no LAN exposure); Next binds the exact -p port or exits,
// so no strict-port wrangling is needed. The client flow is the same
// KindWeb browser proxy — only the spawn recipe differs.
var nextCmdTemplate = bootstrapTemplate(`npx --yes next dev -p %d -H 127.0.0.1`)

// bootstrapTemplate wraps a dev-server exec line in the self-healing
// bun-install bootstrap documented on expoCmdTemplate. The returned argv
// is a Spec.CmdTemplate; a "%d" placeholder inside execLine survives
// verbatim for renderArgs to substitute.
func bootstrapTemplate(execLine string) []string {
	return []string{
		"sh", "-c",
		`m="../.clank-preview-bootstrap/$(basename "$(pwd)").bun"; ` +
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

// webReadyProbe: a bare 200 on / is the only signal Vite gives that's
// framework-independent (a Vite SPA serves index.html; SvelteKit SSR
// renders the page). No body substring — there's nothing stable to
// match across frameworks, and Vite binds the port only when it's
// actually able to serve.
var webReadyProbe = ReadyProbe{
	Path: "/",
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
// or Vite web app. The contract:
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
// Expo wins over Vite when both match (an Expo app with a vite dep is
// almost certainly an Expo app; a Vite web app never carries an Expo
// app config), and Next wins over Vite (a Next project's dev server is
// `next dev` even when vite appears as a transitive tool, e.g. for
// vitest). Detection is intentionally cheap (one Stat for
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
			Kind:        KindExpo,
			CmdTemplate: append([]string(nil), expoCmdTemplate...),
			ReadyProbe:  expoReadyProbe,
		}, nil
	}

	if packageHasDep(data, "next") {
		return &Spec{
			Kind:        KindWeb,
			CmdTemplate: append([]string(nil), nextCmdTemplate...),
			ReadyProbe:  webReadyProbe,
		}, nil
	}

	if packageHasDep(data, "vite") {
		return &Spec{
			Kind:        KindWeb,
			CmdTemplate: append([]string(nil), webCmdTemplate...),
			ReadyProbe:  webReadyProbe,
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
// in either dependencies or devDependencies (vite lives in
// devDependencies in virtually every template; expo templates split).
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
