package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// When the guest has no metro.config.js, ensurePreviewRuntime writes both our
// runtime file and a standalone config that registers it as a premodule.
func TestEnsurePreviewRuntime_NoExistingConfig(t *testing.T) {
	dir := t.TempDir()

	if err := ensurePreviewRuntime(dir); err != nil {
		t.Fatalf("ensurePreviewRuntime: %v", err)
	}

	rt, err := os.ReadFile(filepath.Join(dir, previewRuntimeFile))
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if string(rt) != previewRuntimeJS {
		t.Errorf("%s does not match embedded runtime", previewRuntimeFile)
	}

	cfg, err := os.ReadFile(filepath.Join(dir, metroConfigFile))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, want := range []string{injectMarker, "getDefaultConfig", "require.resolve('./clank-preview-runtime')"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("standalone metro.config.js missing %q; got:\n%s", want, cfg)
		}
	}
}

// When the guest already has a metro.config.js, ensurePreviewRuntime appends
// its wrapper without disturbing the user's config, and is idempotent across
// repeated /preview/start calls.
func TestEnsurePreviewRuntime_ExistingConfigAppendsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	original := "const { getDefaultConfig } = require('expo/metro-config');\n" +
		"const { withNativeWind } = require('nativewind/metro');\n" +
		"const config = getDefaultConfig(__dirname);\n" +
		"module.exports = withNativeWind(config, { input: './global.css' });\n"
	cfgPath := filepath.Join(dir, metroConfigFile)
	if err := os.WriteFile(cfgPath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := ensurePreviewRuntime(dir); err != nil {
		t.Fatalf("ensurePreviewRuntime (first): %v", err)
	}

	afterFirst, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(afterFirst)
	if !strings.HasPrefix(got, original) {
		t.Errorf("user's original config was not preserved verbatim at the top; got:\n%s", got)
	}
	if !strings.Contains(got, "require.resolve('./clank-preview-runtime')") {
		t.Errorf("appended wrapper missing require.resolve; got:\n%s", got)
	}
	if n := strings.Count(got, injectMarker); n != 1 {
		t.Errorf("want marker exactly once after first run, got %d", n)
	}

	// Second run must be a no-op (no double append).
	if err := ensurePreviewRuntime(dir); err != nil {
		t.Fatalf("ensurePreviewRuntime (second): %v", err)
	}
	afterSecond, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(afterSecond) != got {
		t.Errorf("second run mutated the config (not idempotent):\nbefore:\n%s\nafter:\n%s", got, afterSecond)
	}
	if n := strings.Count(string(afterSecond), injectMarker); n != 1 {
		t.Errorf("want marker exactly once after idempotent re-run, got %d", n)
	}
}

// A config that already carries our marker (e.g. shipped in a template) is left
// untouched.
func TestEnsurePreviewRuntime_AlreadyMarkedUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, metroConfigFile)
	preWired := "// " + injectMarker + "\nmodule.exports = {};\n"
	if err := os.WriteFile(cfgPath, []byte(preWired), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := ensurePreviewRuntime(dir); err != nil {
		t.Fatalf("ensurePreviewRuntime: %v", err)
	}

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != preWired {
		t.Errorf("already-marked config was modified:\nwant:\n%s\ngot:\n%s", preWired, got)
	}
}
