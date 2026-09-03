package webpreview

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestOverlayModulesJS runs the overlay's pure-module tests with the
// system node — real modules, no DOM,
// no mocks. Skips where node isn't installed (same pattern as the
// opencode-dependent agent tests).
func TestOverlayModulesJS(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping overlay module tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "chat_test.mjs", "markdown_test.mjs", "settings_test.mjs", "sourcecontrol_test.mjs", "boxpos_test.mjs", "launcher_test.mjs", "resize_test.mjs", "toplayer_test.mjs")
	cmd.Dir = "overlay"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test overlay modules: %v\n%s", err, out)
	}
}
