package preview

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acksell/clank/internal/config"
)

const (
	// bootstrapMarkerEnv carries the bootstrap completion-marker path
	// from spawn into the bootstrapShell. Env rather than in-shell path
	// assembly: the Go side owns the location (config dir, hash key)
	// and the value needs no shell quoting.
	bootstrapMarkerEnv = "CLANK_PREVIEW_BOOTSTRAP_MARKER"

	// bootstrapInstallerEnv carries the installer identity the shell
	// records in the markers: a Packager name for synthesized installs,
	// the verbatim clank.yaml preview.install command for overrides.
	bootstrapInstallerEnv = "CLANK_PREVIEW_INSTALLER"

	// bootstrapWipeEnv, when non-empty, tells the shell to delete
	// node_modules before installing. The decision is Go's
	// (needsNodeModulesWipe); the multi-second rm -rf itself stays in
	// the async child instead of the Start request path.
	bootstrapWipeEnv = "CLANK_PREVIEW_WIPE_NODE_MODULES"

	// markerInstallingSuffix marks a clank-owned install in flight:
	// "<marker>.installing" is written before the install and removed
	// after the completion marker lands.
	markerInstallingSuffix = ".installing"
)

// bootstrapMarkerPath returns the completion-marker file for workDir's
// dependency bootstrap. Lives under the clank state dir so nothing is
// written inside — or next to — the user's repo; keyed by the
// workdir's basename plus a hash of its absolute path so same-named
// folders (a monorepo's web-app/, sibling clones) can't collide. The
// file's CONTENT records which installer produced the tree.
// TODO(ai-review): key on a stable worktree identity, not just the abs
// path, so a local-checkout import reusing a path can't inherit a prior
// project's packager marker. https://github.com/Acksell/clank/pull/204#discussion_r3684840392
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
	name := fmt.Sprintf("%s-%x", filepath.Base(abs), sum[:6])
	return filepath.Join(dir, "preview-bootstrap", name), nil
}

// bootstrapShell wraps launch in the self-healing dependency
// bootstrap: install, record the completion marker, then launch. A
// materialized worktree only carries what's tracked in git —
// node_modules is gitignored — so the first spawn must install before
// the dev server can start. The install runs unconditionally (a warm
// tree makes it a fast no-op) and its output is deliberately not
// silenced: it streams to the client (ring buffer → /preview/logs) so
// a multi-minute first run shows live progress instead of a blind
// spinner.
//
// Marker protocol (all paths from env — the shell assembles none):
//
//   - "<m>.installing" is written BEFORE the install and removed after
//     success. Present without a completion marker ⇒ clank's first
//     install of this workdir was interrupted and the tree is a
//     half-extracted unknown; needsNodeModulesWipe then orders a clean
//     slate.
//   - "<m>" is written only AFTER a successful install and records
//     WHAT installed ($CLANK_PREVIEW_INSTALLER). A later spawn wipes
//     node_modules only on a proven packager switch — never merely
//     because the marker is absent, which would delete a user's own
//     node_modules on their first preview of an existing project.
//
// Fails fast when either env is missing — a caller bug, never a
// reason to guess. installFragment must leave $? honest; launch is
// used verbatim (synthesized recipes carry their own `exec` prefix,
// clank.yaml commands run as written so compound commands work — the
// Setpgid group kill reaches every child either way).
func bootstrapShell(installFragment, launch string) string {
	return `m="$` + bootstrapMarkerEnv + `"; ` +
		`[ -n "$m" ] || { echo "` + bootstrapMarkerEnv + ` is not set" >&2; exit 1; }; ` +
		`inst="$` + bootstrapInstallerEnv + `"; ` +
		`[ -n "$inst" ] || { echo "` + bootstrapInstallerEnv + ` is not set" >&2; exit 1; }; ` +
		`[ -z "$` + bootstrapWipeEnv + `" ] || rm -rf node_modules; ` +
		`mkdir -p "$(dirname "$m")" && printf '%s' "$inst" > "$m` + markerInstallingSuffix + `" || exit 1; ` +
		installFragment + ` && ` +
		`printf '%s' "$inst" > "$m" && rm -f "$m` + markerInstallingSuffix + `" && ` +
		launch
}

// needsNodeModulesWipe decides whether the bootstrap must delete
// node_modules before installing (returned to the shell via
// bootstrapWipeEnv). installer is the Spec.Installer about to run.
//
// The rules protect two invariants at once: never destroy a tree
// clank didn't build, and never trust a tree a different resolver
// (or a torn install) built:
//
//   - no completion marker: the tree (if any) is the user's own —
//     wipe only when an .installing sentinel proves clank died
//     mid-install and left a half-extracted unknown.
//   - completion marker present: the installer's own re-run reconciles
//     the tree, so wipe only on a proven cross-packager switch (mixed
//     resolver artifacts are exactly what the wipe exists for).
//     Freeform clank.yaml install commands never trigger it — whoever
//     overrides the install owns the tree.
func needsNodeModulesWipe(markerPath, installer string) bool {
	completed, err := os.ReadFile(markerPath)
	if err != nil {
		_, serr := os.Stat(markerPath + markerInstallingSuffix)
		return serr == nil
	}
	prev := strings.TrimSpace(string(completed))
	return prev != installer && isPackagerName(prev) && isPackagerName(installer)
}
