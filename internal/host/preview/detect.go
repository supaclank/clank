package preview

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/clankyaml"
)

// Framework dev-server invocations, without the package-manager launch
// prefix (execLine adds it) and without the install bootstrap
// (bootstrapShell adds it). ${PORT} is the allocated port, substituted
// by renderArgs — the same token clank.yaml commands use, so one
// substitution rule covers both.
//
// expo: `--non-interactive` tells Expo CLI to skip prompts; we
// deliberately do NOT set CI=true (Metro reads CI and disables watch
// mode + HMR). EXPO_NO_DOTENV is set in spawn.buildEnv.
const expoToolArgs = "expo start --port ${PORT} --non-interactive"

// vite: `--strictPort` because Manager allocated the port and the
// readiness probe polls exactly it — Vite's default auto-increment on
// a busy port would leave the probe polling a dead socket until
// timeout. `--host 127.0.0.1` because Vite's default host is
// "localhost", which Node ≥17 may resolve (and bind) as ::1 only —
// observed on macOS — while probeReady and the webpreview proxy dial
// IPv4 loopback. No --clearScreen wrangling needed: Vite only clears
// when stdout is a TTY, and ours is the ring buffer.
const viteToolArgs = "vite --port ${PORT} --strictPort --host 127.0.0.1"

// next: `-H 127.0.0.1` for the same reason as Vite's --host (loopback
// parity with the probe and proxy, and no LAN exposure); Next binds
// the exact -p port or exits, so no strict-port wrangling is needed.
// The client flow is the same KindWeb browser proxy — only the spawn
// recipe differs.
const nextToolArgs = "next dev -p ${PORT} -H 127.0.0.1"

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
// actually able to serve. Also the default probe for clank.yaml
// custom commands, overridable via preview.ready.
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
//   - (nil, err) means I/O blew up reading the worktree, clank.yaml is
//     invalid, or the config asks for something impossible. A user who
//     wrote config gets a loud error, never a silent "not previewable".
//   - (*Spec, nil) means the dev server should be spawnable with the
//     returned recipe.
//
// clank.yaml wins over framework sniffing: preview.dir re-roots
// detection into a subdirectory (monorepos), preview.command bypasses
// detection entirely (KindWeb, the command owns everything but the
// allocated port), preview.install replaces the synthesized
// package-manager install, preview.ready overrides the probe. With no
// config, detection synthesizes the install from the project's own
// package manager (ResolvePackager) so a pnpm/yarn/npm project is
// installed by its own resolver against its own lockfile.
//
// Detection stays cheap (a few Stats and one small JSON parse, plus
// one clank.yaml read) so callers can run it per worktree-list row
// without caching.
func Detect(workDir string) (*Spec, error) {
	cfg, err := clankyaml.Load(workDir)
	if err != nil {
		return nil, err
	}
	var pv *clankyaml.Preview
	if cfg != nil {
		pv = cfg.Preview
	}

	root := workDir
	subdir := ""
	if pv != nil && pv.Dir != "" {
		subdir = filepath.Clean(pv.Dir)
		root = filepath.Join(workDir, subdir)
		if fi, statErr := os.Stat(root); statErr != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%s: preview.dir %q is not a directory under %s", clankyaml.FileName, pv.Dir, workDir)
		}
	}

	if pv != nil && pv.Command != "" {
		return customSpec(pv, subdir), nil
	}

	match, err := detectFramework(root)
	if err != nil {
		return nil, err
	}
	if match == nil {
		if pv != nil && previewConfigured(pv) {
			// The user explicitly configured a preview; "nothing to
			// run" must fail loudly, not render a missing button.
			return nil, fmt.Errorf("%s: preview is configured but no supported framework was detected in %s and no preview.command is set", clankyaml.FileName, root)
		}
		return nil, nil
	}

	pm, evidence, err := ResolvePackager(root)
	if err != nil {
		return nil, err
	}

	install := installFragment(pm)
	installer := string(pm)
	requiredTool := string(pm)
	if pv != nil && pv.Install != "" {
		install = pv.Install
		installer = pv.Install
		// bun and yarn also LAUNCH the tool, so they stay required with
		// an install override; npm/pnpm exec node_modules/.bin directly
		// and are only needed for the install they no longer run.
		if pm == PackagerNPM || pm == PackagerPNPM {
			requiredTool = ""
		}
	}

	probe := match.probe
	if pv != nil && pv.Ready != nil {
		probe = ReadyProbe{Path: pv.Ready.Path, ExpectedSubstr: pv.Ready.Expect}
	}

	return &Spec{
		Kind:         match.kind,
		CmdTemplate:  []string{"sh", "-c", bootstrapShell(install, execLine(pm, match.toolArgs))},
		PortToken:    clankyaml.PortPlaceholder,
		Dir:          subdir,
		Installer:    installer,
		RequiredTool: requiredTool,
		ToolEvidence: evidence,
		ReadyProbe:   probe,
	}, nil
}

// customSpec builds the Spec for a clank.yaml preview.command: always
// the KindWeb browser flow, detection fully bypassed. Without a
// preview.install the command owns its own dependencies (no bootstrap,
// no marker); with one, the install runs behind the same marker
// protocol as synthesized recipes.
//
// No `exec` prefix on user commands — a compound command (a && b)
// would never run past the exec, and teardown doesn't rely on it: the
// Setpgid group kill reaches every child either way.
func customSpec(pv *clankyaml.Preview, subdir string) *Spec {
	probe := webReadyProbe
	if pv.Ready != nil {
		probe = ReadyProbe{Path: pv.Ready.Path, ExpectedSubstr: pv.Ready.Expect}
	}
	spec := &Spec{
		Kind:       KindWeb,
		PortToken:  clankyaml.PortPlaceholder,
		Dir:        subdir,
		ReadyProbe: probe,
	}
	if pv.Install != "" {
		spec.Installer = pv.Install
		spec.CmdTemplate = []string{"sh", "-c", bootstrapShell(pv.Install, pv.Command)}
	} else {
		spec.CmdTemplate = []string{"sh", "-c", pv.Command}
	}
	return spec
}

// previewConfigured reports whether the user set anything in the
// preview section (an empty `preview:` block reads as absent).
// Command is handled before this is consulted.
func previewConfigured(pv *clankyaml.Preview) bool {
	return pv.Dir != "" || pv.Install != "" || pv.Ready != nil
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
