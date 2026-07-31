// Package clankyaml loads the per-repo clank.yaml config file.
//
// clank.yaml lives at the directory the user previews from (the repo
// or worktree root) and is written by the user, never by clank. The
// file is organized as independent top-level sections so future
// features (e.g. agent config) can join without a schema break:
// unknown top-level sections are tolerated, unknown keys INSIDE a
// known section are errors (a typo'd key must fail loudly, not
// silently no-op).
package clankyaml

import "gopkg.in/yaml.v3"

// FileName is the config file name looked up at the preview root.
const FileName = "clank.yaml"

// PortPlaceholder is the literal token a preview.command must contain.
// clank allocates the dev-server port and substitutes every occurrence
// at spawn time; a command without it would listen somewhere the
// readiness probe and proxy never look.
const PortPlaceholder = "${PORT}"

// File is the parsed clank.yaml document.
type File struct {
	Preview *Preview `yaml:"preview"`

	// rest absorbs unknown top-level sections so a clank binary older
	// than a repo's config keeps working. Intentionally unexported:
	// nothing reads it — it exists so strict decoding doesn't reject
	// future sections.
	Rest map[string]yaml.Node `yaml:",inline"`
}

// Preview configures `clank preview` for this repo. Every field is
// optional; an absent file (or section) means full auto-detection.
type Preview struct {
	// Dir is the repo-relative subdirectory to preview from (monorepos:
	// "web-app"). Detection, installs, and the dev server all run
	// there. Must stay inside the repo.
	Dir string `yaml:"dir"`

	// Install is the dependency-install command, run through `sh -c`
	// in the preview dir before the dev server starts (gated by the
	// bootstrap completion marker; see internal/host/preview). It
	// replaces the package-manager install clank would otherwise
	// synthesize from the repo's lockfile.
	Install string `yaml:"install"`

	// Command is the dev-server command, run through `sh -c` in the
	// preview dir. Must contain PortPlaceholder. Setting it makes the
	// preview a browser (web) preview and bypasses framework
	// detection entirely; the command owns everything but the port.
	// The server must bind 127.0.0.1 on the substituted port — the
	// readiness probe and the overlay proxy dial IPv4 loopback
	// (Node ≥ 17 may resolve "localhost" as ::1 only).
	Command string `yaml:"command"`

	// Ready overrides the readiness probe for the spawned server.
	Ready *Ready `yaml:"ready"`
}

// Ready describes the HTTP readiness probe: GET Path on the allocated
// port until it returns 200 and (when Expect is set) the body contains
// Expect.
type Ready struct {
	Path   string `yaml:"path"`
	Expect string `yaml:"expect"`
}
