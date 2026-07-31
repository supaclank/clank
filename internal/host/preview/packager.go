package preview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Packager identifies which package manager installs a project's
// dependencies and launches its dev tools. The set is closed: these
// are the four whose install/exec conventions the preview bootstrap
// knows how to synthesize.
type Packager string

const (
	PackagerBun  Packager = "bun"
	PackagerNPM  Packager = "npm"
	PackagerPNPM Packager = "pnpm"
	PackagerYarn Packager = "yarn"
)

// PackagerPolicy selects how Detect picks the installer for a
// recognized framework. It's a construction-time choice made by
// whoever wires the host — never sniffed from the environment.
type PackagerPolicy string

const (
	// PackagerPolicyReuseProject installs with the project's own
	// manager: the user's saved per-project choice when one exists,
	// else detection via ResolvePackager. The laptop posture — the
	// preview runs against the user's own checkout, so respect its
	// resolver and lockfile. Also the zero-value behavior: it's the
	// side that never surprises a repo.
	PackagerPolicyReuseProject PackagerPolicy = "reuse-project"

	// PackagerPolicyAlwaysBun installs with bun regardless of the
	// repo. The cloud posture: machine worktrees are materialized
	// fresh from git (no user tree to protect), and bun's speed and
	// hard-linked disk usage are what an I/O-constrained machine
	// needs.
	PackagerPolicyAlwaysBun PackagerPolicy = "always-bun"
)

// ParsePackagerPolicy validates a policy string from a flag. Unknown
// values are a deploy bug — fail the boot, don't guess.
func ParsePackagerPolicy(s string) (PackagerPolicy, error) {
	switch PackagerPolicy(s) {
	case PackagerPolicyReuseProject, PackagerPolicyAlwaysBun:
		return PackagerPolicy(s), nil
	}
	return "", fmt.Errorf("unknown packager policy %q (want %q or %q)", s, PackagerPolicyReuseProject, PackagerPolicyAlwaysBun)
}

// isPackagerName reports whether s is one of the known Packager
// values. Used to tell detected-PM marker records apart from freeform
// clank.yaml install commands.
func isPackagerName(s string) bool {
	switch Packager(s) {
	case PackagerBun, PackagerNPM, PackagerPNPM, PackagerYarn:
		return true
	}
	return false
}

// packagerLockfiles maps lockfile names to their owner, in detection
// order. bun first: a bun.lock next to a package-lock.json is almost
// always a bun migration artifact of a repo that has since committed
// to bun.
var packagerLockfiles = []struct {
	file string
	pm   Packager
}{
	{"bun.lock", PackagerBun},
	{"bun.lockb", PackagerBun},
	{"pnpm-lock.yaml", PackagerPNPM},
	{"yarn.lock", PackagerYarn},
	{"package-lock.json", PackagerNPM},
}

// ResolvePackager determines which package manager dir's project uses.
// Walks from dir up to (and including) the first directory containing
// .git — monorepos keep the lockfile at the repo root while the app
// lives in a subdirectory — checking at each level:
//
//  1. package.json's "packageManager" field (the corepack convention,
//     e.g. "pnpm@9.1.0") — authoritative when present. A manager
//     outside the supported four is an error, not a silent fallback.
//  2. package.json's newer devEngines.packageManager shape.
//  3. Lockfiles (packagerLockfiles).
//
// No signal anywhere → bun with empty evidence: the default for
// clank-created template projects, which ship no lockfile.
//
// The evidence string names what decided (field value or lockfile,
// plus the directory when it wasn't dir itself) for error messages
// and the CLI's install-speed nudge.
func ResolvePackager(dir string) (Packager, string, error) {
	cur := dir
	for {
		pm, evidence, err := packagerSignalIn(cur)
		if err != nil {
			return "", "", err
		}
		if pm != "" {
			if cur != dir {
				if rel, relErr := filepath.Rel(dir, cur); relErr == nil {
					evidence += " in " + rel
				}
			}
			return pm, evidence, nil
		}
		if isRepoRoot(cur) {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return PackagerBun, "", nil
}

// packagerSignalIn checks a single directory for a package-manager
// signal. Empty Packager with nil error means "no signal here".
func packagerSignalIn(dir string) (Packager, string, error) {
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		pm, evidence, perr := packagerFromPackageJSON(data)
		if perr != nil {
			return "", "", perr
		}
		if pm != "" {
			return pm, evidence, nil
		}
	}
	for _, lf := range packagerLockfiles {
		if _, err := os.Stat(filepath.Join(dir, lf.file)); err == nil {
			return lf.pm, lf.file, nil
		}
	}
	return "", "", nil
}

// packagerFromPackageJSON reads the packageManager / devEngines
// declarations. Malformed JSON is treated as "no signal" — the same
// tolerance Detect's dependency sniffing applies, since a project
// with a broken package.json couldn't run its dev server either.
func packagerFromPackageJSON(data []byte) (Packager, string, error) {
	var pkg struct {
		PackageManager string `json:"packageManager"`
		DevEngines     struct {
			PackageManager json.RawMessage `json:"packageManager"`
		} `json:"devEngines"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", "", nil
	}
	if pkg.PackageManager != "" {
		name, _, _ := strings.Cut(pkg.PackageManager, "@")
		if !isPackagerName(name) {
			return "", "", fmt.Errorf("package.json declares packageManager %q — clank previews support %s; set preview.install in clank.yaml to use it", pkg.PackageManager, supportedPackagerList())
		}
		return Packager(name), fmt.Sprintf("package.json packageManager %q", pkg.PackageManager), nil
	}
	if len(pkg.DevEngines.PackageManager) > 0 {
		name := devEnginesPackagerName(pkg.DevEngines.PackageManager)
		if name != "" {
			if !isPackagerName(name) {
				return "", "", fmt.Errorf("package.json declares devEngines.packageManager %q — clank previews support %s; set preview.install in clank.yaml to use it", name, supportedPackagerList())
			}
			return Packager(name), fmt.Sprintf("package.json devEngines.packageManager %q", name), nil
		}
	}
	return "", "", nil
}

// devEnginesPackagerName extracts the manager name from the
// devEngines.packageManager value, which the spec allows as either a
// single object or an array of them (first entry wins).
func devEnginesPackagerName(raw json.RawMessage) string {
	type engine struct {
		Name string `json:"name"`
	}
	var one engine
	if err := json.Unmarshal(raw, &one); err == nil && one.Name != "" {
		return one.Name
	}
	var many []engine
	if err := json.Unmarshal(raw, &many); err == nil && len(many) > 0 {
		return many[0].Name
	}
	return ""
}

func supportedPackagerList() string {
	return fmt.Sprintf("%s, %s, %s, and %s", PackagerNPM, PackagerPNPM, PackagerYarn, PackagerBun)
}

// isRepoRoot reports whether dir is a git repo or worktree root (.git
// is a directory in a normal checkout, a file in a linked worktree).
func isRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// installFragment returns the shell fragment that installs
// dependencies with pm, leaving $? honest for the bootstrap chain.
//
// Flags are deliberately the lockfile-respecting, non-destructive
// variants: plain `npm install` (NOT `npm ci`, which wipes
// node_modules by design), plain `pnpm install` / `yarn install`.
// Frozen-lockfile flags would hard-fail unattended previews of repos
// whose lockfile has drifted, so we accept the manager's default
// reconcile behavior instead.
//
// bun keeps its historical dance: `--no-save` stops bun touching
// package.json, but when the repo has a package-lock.json bun migrates
// it and writes a bun.lock EVEN under --no-save (verified on bun
// 1.3.11/1.3.14; no flag disables it), so the fragment deletes the
// migrated bun.lock afterward unless one existed before the install.
func installFragment(pm Packager) string {
	switch pm {
	case PackagerBun:
		return `keep_lock=; [ -f bun.lock ] && keep_lock=1; ` +
			`bun install --no-save; err=$?; ` +
			`[ -n "$keep_lock" ] || rm -f bun.lock; ` +
			`[ "$err" -eq 0 ]`
	case PackagerNPM:
		return "npm install"
	case PackagerPNPM:
		return "pnpm install"
	case PackagerYarn:
		return "yarn install"
	}
	// Unreachable for the closed enum; loud beats a silent bad spawn.
	return "echo 'unknown packager " + string(pm) + "' >&2; false"
}

// execLine returns the shell line that launches toolAndArgs (e.g.
// "expo start --port %d") under pm, prefixed with `exec` so signals +
// Setpgid target the dev server directly.
//
//   - bun runs the worktree-local bin and honors its `#!/usr/bin/env
//     node` shebang — required, because the preview shim rides
//     NODE_OPTIONS (see spawn.buildEnv), which only Node honors.
//   - npm/pnpm exec node_modules/.bin directly: deterministic
//     (`npx` would fall back to fetching from the registry when the
//     bin is missing) and the shebang lands on Node the same way.
//   - yarn must run `yarn <tool>`: yarn PnP has no node_modules/.bin,
//     and `yarn` injects its resolver for both classic and berry.
func execLine(pm Packager, toolAndArgs string) string {
	switch pm {
	case PackagerBun:
		return "exec bun " + toolAndArgs
	case PackagerNPM, PackagerPNPM:
		// TODO(ai-review): a tool hoisted to a workspace root's
		// node_modules/.bin (root-only devDependency in an npm/pnpm
		// workspace) isn't found here — only workDir's own .bin is
		// searched. https://github.com/Acksell/clank/pull/205#discussion_r3690349688
		return "exec node_modules/.bin/" + toolAndArgs
	case PackagerYarn:
		return "exec yarn " + toolAndArgs
	}
	return "echo 'unknown packager " + string(pm) + "' >&2; false"
}
