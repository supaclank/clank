package tui

import (
	"os"
	"testing"
)

// TestMain isolates the test process from the developer's real ~/.clank
// preferences. Several views read config.LoadPreferences() at construction —
// the compose view resolves the default backend from prefs.DefaultBackend, and
// the inbox derives its sidebar width from prefs.SidebarWidthRatio. Without
// isolation a local preferences.json (e.g. default_backend=claude-code, or a
// custom sidebar_width_ratio) silently overrides the built-in defaults, so
// tests that assert those defaults pass or fail depending on whose machine
// they run on. Pointing CLANK_DIR at an empty temp dir makes LoadPreferences
// return zero-value prefs → built-in defaults (opencode backend, default
// sidebar ratio), keeping the suite deterministic. CLANK_DIR is the documented
// override consulted by config.Dir (see config.preferencesPath).
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// TODO(ai-review): all tests share this dir; if a future test writes prefs,
// use t.Setenv("CLANK_DIR", t.TempDir()) per-test instead.
// https://github.com/supaclank/clank/pull/76#discussion_r3449143069
func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "clank-tui-test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	if err := os.Setenv("CLANK_DIR", dir); err != nil {
		panic(err)
	}
	return m.Run()
}
