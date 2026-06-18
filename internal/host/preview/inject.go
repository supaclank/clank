package preview

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// previewRuntimeJS is clank-preview-runtime.js, embedded into the host binary
// and written into each guest project before `expo start` runs. It's the
// user-facing half of "no scary error popups in the preview": running as a
// Metro premodule it calls LogBox.ignoreAllLogs + installs an ErrorUtils
// global handler that swallows fatals before RN's fullscreen error screen and
// reports a friendly summary to the clank-mobile native overlay. See
// preview-runtime.js for the full rationale.
//
//go:embed preview-runtime.js
var previewRuntimeJS string

const (
	// previewRuntimeFile is written into the guest project root. The user
	// never edits it, so we always overwrite with the embedded copy.
	previewRuntimeFile = "clank-preview-runtime.js"

	// metroConfigFile is Metro's project-root config. Expo's CLI loads it
	// automatically on `expo start`; we wire it to register our premodule.
	metroConfigFile = "metro.config.js"

	// metroConfigCjsFile is the ESM-compatible variant Metro also accepts.
	// Projects with "type":"module" in package.json must use this name.
	metroConfigCjsFile = "metro.config.cjs"

	// injectMarker guards against re-appending our wrapper on repeat
	// /preview/start calls and lets a human spot the auto-managed block.
	injectMarker = "clank-preview-runtime (auto-injected)"
)

// standaloneMetroConfig is written when the guest project has no
// metro.config.js of its own. Built on Expo's defaults so a plain Expo app
// keeps working.
const standaloneMetroConfig = `// ` + injectMarker + `
const { getDefaultConfig } = require('expo/metro-config');
const config = getDefaultConfig(__dirname);
config.serializer = config.serializer || {};
const __clankPrev = config.serializer.getModulesRunBeforeMainModule;
config.serializer.getModulesRunBeforeMainModule = () => [
  ...(__clankPrev ? __clankPrev() : []),
  require.resolve('./clank-preview-runtime'),
];
module.exports = config;
`

// metroConfigAppendSnippet is appended to an EXISTING metro.config.js/cjs. It
// runs after the file's own `module.exports = <config>`, so the exported value
// already exists. Handles both object exports (most Expo projects) and function
// exports (monorepos / custom config wrappers). Purely additive, idempotent via
// the marker, and wrapped in try/catch so an unexpected export shape can never
// break the user's bundler.
const metroConfigAppendSnippet = `
// ` + injectMarker + `: register clank-preview-runtime.js as a Metro premodule
// so the guest silences React Native's dev error UI in the clank preview.
// Additive + auto-managed; delete this block to opt out.
try {
  function __clankWrap(cfg) {
    if (!cfg || typeof cfg !== 'object') return;
    cfg.serializer = cfg.serializer || {};
    var prev = cfg.serializer.getModulesRunBeforeMainModule;
    cfg.serializer.getModulesRunBeforeMainModule = function () {
      return (prev ? prev.apply(this, arguments) : []).concat([
        require.resolve('./clank-preview-runtime'),
      ]);
    };
  }
  var __clankCfg = module.exports;
  if (typeof __clankCfg === 'function') {
    module.exports = function () { var c = __clankCfg.apply(this, arguments); __clankWrap(c); return c; };
  } else {
    __clankWrap(__clankCfg);
  }
} catch (e) {}
`

// ensurePreviewRuntime makes the guest project at workDir silence React
// Native's developer error UI during a preview: it writes
// clank-preview-runtime.js and ensures metro.config.js registers it as a Metro
// premodule. `expo start` reads metro.config.js at startup, so this MUST run
// before the dev server is spawned.
//
// Idempotent — safe to call on every /preview/start (a re-run overwrites our
// runtime file and skips an already-wired config). Best-effort by contract:
// callers treat a failure as non-fatal (the preview still runs; the native
// host still suppresses the redbox — only the guest-side LogBox suppression +
// the "Fixing a glitch…" signal are lost).
func ensurePreviewRuntime(workDir string) error {
	// TODO(ai-review): use temp-file+rename for atomic writes so concurrent starts
	// on the same workDir can't produce a torn config read by Metro at startup.
	// https://github.com/Acksell/clank/pull/65#discussion_r3439529602

	// 1. Drop our runtime module (overwrite — it's ours, never user-edited).
	rtPath := filepath.Join(workDir, previewRuntimeFile)
	if err := os.WriteFile(rtPath, []byte(previewRuntimeJS), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", previewRuntimeFile, err)
	}

	// 2. Wire metro.config.js (or .cjs for ESM projects) to run our premodule.
	cfgPath, existing, err := readMetroConfig(workDir)
	if err != nil {
		return err
	}
	if cfgPath == "" {
		// No config yet — write a standalone one on Expo's defaults.
		p := filepath.Join(workDir, metroConfigFile)
		if err := os.WriteFile(p, []byte(standaloneMetroConfig), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", metroConfigFile, err)
		}
		return nil
	}

	// 3. Already wired by a previous call — nothing to do.
	if strings.Contains(existing, injectMarker) {
		return nil
	}

	// 4. Append our additive wrapper.
	out := existing
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += metroConfigAppendSnippet
	if err := os.WriteFile(cfgPath, []byte(out), 0o644); err != nil {
		return fmt.Errorf("append %s: %w", filepath.Base(cfgPath), err)
	}
	return nil
}

// readMetroConfig looks for metro.config.js or metro.config.cjs in workDir,
// returning the path and contents of whichever exists first. Returns ("", "",
// nil) when neither exists.
func readMetroConfig(workDir string) (path, contents string, err error) {
	for _, name := range []string{metroConfigFile, metroConfigCjsFile} {
		p := filepath.Join(workDir, name)
		data, readErr := os.ReadFile(p)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return "", "", fmt.Errorf("read %s: %w", name, readErr)
		}
		return p, string(data), nil
	}
	return "", "", nil
}
