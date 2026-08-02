package webpreview

import (
	"os/exec"
	"testing"
)

// TestOverlayModulesJS runs the overlay's pure-module tests with the
// system node — real modules, no DOM,
// no mocks. Skips where node isn't installed (same pattern as the
// opencode-dependent agent tests).
func TestOverlayModulesJS(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping overlay chat.js tests")
	}
	cmd := exec.Command(node, "--test", "chat_test.mjs", "settings_test.mjs")
	cmd.Dir = "overlay"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test overlay modules: %v\n%s", err, out)
	}
}
