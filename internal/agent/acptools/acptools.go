// Package acptools provisions the pinned ACP adapter packages onto a
// host. The embedded manifest (package.json + bun.lock) is materialized
// into the tools dir and installed with bun --frozen-lockfile, so every
// host runs exactly the reviewed dependency graph — including the exact
// @openai/codex the adapter's caret range would otherwise float.
package acptools

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

//go:embed manifest/package.json manifest/bun.lock
var manifestFS embed.FS

// Pinned adapter versions. A unit test asserts these match the embedded
// manifest so a pin bump is always a reviewed two-file diff.
const (
	PinnedCodexACPVersion  = "1.1.7"
	PinnedCodexVersion     = "0.145.0"
	PinnedClaudeACPVersion = "0.61.0"
)

// installTimeout bounds the cold-cache bun install (the codex platform
// binary is ~100MB; first-ever codex session on a slow link needs room).
const installTimeout = 3 * time.Minute

// Paths locates the provisioned tools for spawning.
type Paths struct {
	Dir            string
	BunBin         string
	CodexACPEntry  string
	ClaudeACPEntry string
}

var (
	ensureMu   sync.Mutex
	ensureDone = map[string]Paths{}
)

// Ensure materializes the manifest into toolsDir and installs it with
// bun when missing or changed. Idempotent and serialized; concurrent
// spawn attempts share one install. bun resolves from PATH — a missing
// bun fails here with an actionable error, not at host startup.
func Ensure(ctx context.Context, toolsDir string) (Paths, error) {
	ensureMu.Lock()
	defer ensureMu.Unlock()

	if p, ok := ensureDone[toolsDir]; ok && entryExists(p) {
		return p, nil
	}

	bunBin, err := exec.LookPath("bun")
	if err != nil {
		return Paths{}, fmt.Errorf("the codex backend needs bun to run its ACP adapter: install bun (https://bun.sh) and retry: %w", err)
	}

	changed, err := materializeManifest(toolsDir)
	if err != nil {
		return Paths{}, err
	}
	p := Paths{
		Dir:            toolsDir,
		BunBin:         bunBin,
		CodexACPEntry:  filepath.Join(toolsDir, "node_modules", "@agentclientprotocol", "codex-acp", "dist", "index.js"),
		ClaudeACPEntry: filepath.Join(toolsDir, "node_modules", "@agentclientprotocol", "claude-agent-acp", "dist", "index.js"),
	}
	if changed || !entryExists(p) {
		installCtx, cancel := context.WithTimeout(ctx, installTimeout)
		defer cancel()
		cmd := exec.CommandContext(installCtx, bunBin, "install", "--frozen-lockfile")
		cmd.Dir = toolsDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return Paths{}, fmt.Errorf("bun install acp tools in %s: %w: %s", toolsDir, err, tail(out, 400))
		}
	}
	if missing := firstMissingEntry(p); missing != "" {
		return Paths{}, fmt.Errorf("acp tools install finished but %s is missing", missing)
	}
	ensureDone[toolsDir] = p
	return p, nil
}

// materializeManifest writes the embedded manifest files, reporting
// whether anything changed (which forces a reinstall).
func materializeManifest(toolsDir string) (bool, error) {
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return false, err
	}
	changed := false
	for _, name := range []string{"package.json", "bun.lock"} {
		want, err := manifestFS.ReadFile("manifest/" + name)
		if err != nil {
			return false, err
		}
		dst := filepath.Join(toolsDir, name)
		have, err := os.ReadFile(dst)
		if err == nil && bytes.Equal(have, want) {
			continue
		}
		if err := os.WriteFile(dst, want, 0o644); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func entryExists(p Paths) bool {
	return firstMissingEntry(p) == ""
}

// firstMissingEntry returns the path of whichever adapter entry point
// doesn't exist yet, or "" if both are present.
func firstMissingEntry(p Paths) string {
	for _, entry := range []string{p.CodexACPEntry, p.ClaudeACPEntry} {
		if _, err := os.Stat(entry); err != nil {
			return entry
		}
	}
	return ""
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "…" + string(b[len(b)-n:])
}
