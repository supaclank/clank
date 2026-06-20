package preview

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// clankMetroShimJS is the Node preload (NODE_OPTIONS=--require) that monkeypatches
// Metro in-memory to inject previewRuntimeJS into every guest bundle — without
// writing anything into the user's repo. See clank-metro-shim.js.
//
//go:embed clank-metro-shim.js
var clankMetroShimJS string

// previewRuntimeJS is the guest-side runtime the shim injects: it silences RN's
// dev error UI and reports errors (with their message) to clank-mobile's native
// overlay. See preview-runtime.js.
//
//go:embed preview-runtime.js
var previewRuntimeJS string

const (
	shimFileName    = "clank-metro-shim.js"
	runtimeFileName = "clank-preview-runtime.js"
)

// ensurePreviewShim writes the Metro shim + the preview runtime to a stable temp
// dir OUTSIDE any guest repo and returns their absolute paths. The shim is
// preloaded into `expo start` via NODE_OPTIONS=--require (spawn.buildEnv); it
// injects the runtime into every guest bundle in-memory, so nothing is ever
// written into the user's project.
//
// Idempotent — overwrites on each call (the files are ours, never user-edited).
// The dir is realpath-resolved: Metro canonicalizes watch roots to realpaths, and
// a symlinked root (e.g. macOS /tmp → /private/tmp) could anchor resolution on the
// realpath and surprise us.
func ensurePreviewShim() (shimPath, runtimePath string, err error) {
	base := filepath.Join(os.TempDir(), "clank-preview")
	if mkErr := os.MkdirAll(base, 0o755); mkErr != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", base, mkErr)
	}
	if resolved, rerr := filepath.EvalSymlinks(base); rerr == nil {
		base = resolved
	}

	shimPath = filepath.Join(base, shimFileName)
	runtimePath = filepath.Join(base, runtimeFileName)

	if wErr := os.WriteFile(shimPath, []byte(clankMetroShimJS), 0o644); wErr != nil {
		return "", "", fmt.Errorf("write %s: %w", shimFileName, wErr)
	}
	if wErr := os.WriteFile(runtimePath, []byte(previewRuntimeJS), 0o644); wErr != nil {
		return "", "", fmt.Errorf("write %s: %w", runtimeFileName, wErr)
	}
	return shimPath, runtimePath, nil
}
