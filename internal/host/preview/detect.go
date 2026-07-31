package preview

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Framework dev-server invocations, without the package-manager launch
// prefix (execLine adds it) and without the install bootstrap
// (bootstrapShell adds it). "%d" is the allocated port, substituted by
// renderArgs.
//
// expo: `--non-interactive` tells Expo CLI to skip prompts; we
// deliberately do NOT set CI=true (Metro reads CI and disables watch
// mode + HMR). EXPO_NO_DOTENV is set in spawn.buildEnv.
const expoToolArgs = "expo start --port %d --non-interactive"

// vite: `--strictPort` because Manager allocated the port and the
// readiness probe polls exactly it — Vite's default auto-increment on
// a busy port would leave the probe polling a dead socket until
// timeout. `--host 127.0.0.1` because Vite's default host is
// "localhost", which Node ≥17 may resolve (and bind) as ::1 only —
// observed on macOS — while probeReady and the webpreview proxy dial
// IPv4 loopback. No --clearScreen wrangling needed: Vite only clears
// when stdout is a TTY, and ours is the ring buffer.
const viteToolArgs = "vite --port %d --strictPort --host 127.0.0.1"

// next: `-H 127.0.0.1` for the same reason as Vite's --host (loopback
// parity with the probe and proxy, and no LAN exposure); Next binds
// the exact -p port or exits, so no strict-port wrangling is needed.
// The client flow is the same KindWeb browser proxy — only the spawn
// recipe differs.
const nextToolArgs = "next dev -p %d -H 127.0.0.1"

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

// webReadyProbe: a bare 200 on / is the only signal that's
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

// Detect inspects workDir and returns the Spec Manager uses to spawn
// its dev server. The contract:
//
//   - (nil, nil) means "not previewable" — a normal answer, NOT an
//     error. Surface it as preview_available: false / available: false
//     to the client.
//   - (nil, err) means I/O blew up reading the worktree, or the
//     project declares a package manager clank can't drive. The
//     caller should log and surface it.
//   - (*Spec, nil) means the dev server should be spawnable with the
//     returned recipe.
//
// Install and launch are resolved independently. The INSTALL follows
// policy — reuse-project uses the project's own manager
// (ResolvePackager) with the shared-checkout posture (skip when
// dependencies are already present; frozen mode when creating a
// missing tree from a lockfile, so drift fails loudly instead of
// dirtying the checkout), always-bun pins bun's reconciling install
// for the cloud's owned trees. The LAUNCH derives from the repo's
// declared manager and dependency layout (launchLine), never from who
// installs.
//
// Detection stays cheap (a few Stats and small reads) so callers can
// run it per worktree-list row without caching.
func Detect(workDir string, policy PackagerPolicy) (*Spec, error) {
	match, err := detectFramework(workDir)
	if err != nil || match == nil {
		return nil, err
	}

	pm, evidence, err := installPackagerFor(workDir, policy)
	if err != nil {
		return nil, err
	}

	shared := policy != PackagerPolicyAlwaysBun
	frozen := shared && hasLockfileFor(workDir, pm)
	launch := launchLine(launchViaYarn(workDir), match.toolArgs)

	return &Spec{
		Kind:         match.kind,
		CmdTemplate:  []string{"sh", "-c", bootstrapShell(installFragment(pm, frozen), launch, shared)},
		Installer:    string(pm),
		RequiredTool: string(pm),
		ToolEvidence: evidence,
		ReadyProbe:   match.probe,
	}, nil
}

// installPackagerFor picks the installer under policy. The empty
// policy is the reuse-project zero value (the never-surprise-a-repo
// side), so a bare Options{} manager and tests behave safely.
func installPackagerFor(workDir string, policy PackagerPolicy) (Packager, string, error) {
	switch policy {
	case PackagerPolicyAlwaysBun:
		return PackagerBun, "", nil
	case PackagerPolicyReuseProject, "":
		return ResolvePackager(workDir)
	}
	return "", "", fmt.Errorf("unknown packager policy %q", policy)
}

// hasLockfileFor reports whether dir itself carries pm's lockfile —
// the gate for frozen installs. Deliberately not the walk-up:
// monorepos with a root-level lockfile need the workspace-aware
// reconciling install, and frozen modes generally refuse to run from
// a workspace child anyway.
func hasLockfileFor(dir string, pm Packager) bool {
	for _, lf := range packagerLockfiles {
		if lf.pm != pm {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, lf.file)); err == nil {
			return true
		}
	}
	return false
}

// frameworkMatch is a recognized framework: what to launch and how to
// know it's up. The package-manager half is resolved separately.
type frameworkMatch struct {
	kind     Kind
	toolArgs string
	probe    ReadyProbe
}

// detectFramework classifies dir as an Expo, Next.js, or Vite project,
// or nil for none. Expo wins over Vite when both match (an Expo app
// with a vite dep is almost certainly an Expo app; a Vite web app
// never carries an Expo app config), and Next wins over Vite (a Next
// project's dev server is `next dev` even when vite appears as a
// transitive tool, e.g. for vitest).
func detectFramework(dir string) (*frameworkMatch, error) {
	pkgPath := filepath.Join(dir, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", pkgPath, err)
	}

	if packageHasExpo(data) && hasExpoAppConfig(dir) {
		return &frameworkMatch{kind: KindExpo, toolArgs: expoToolArgs, probe: expoReadyProbe}, nil
	}
	if packageHasDep(data, "next") {
		return &frameworkMatch{kind: KindWeb, toolArgs: nextToolArgs, probe: webReadyProbe}, nil
	}
	if packageHasDep(data, "vite") {
		return &frameworkMatch{kind: KindWeb, toolArgs: viteToolArgs, probe: webReadyProbe}, nil
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
