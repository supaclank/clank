package webpreview

import (
	"os/exec"
	"testing"
)

// TestOverlayChatJS runs the overlay's chat-protocol tests
// (overlay/chat_test.mjs) with the system node — real module, no DOM,
// no mocks. Skips where node isn't installed (same pattern as the
// opencode-dependent agent tests).
func TestOverlayChatJS(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping overlay chat.js tests")
	}
	cmd := exec.Command(node, "--test", "chat_test.mjs")
	cmd.Dir = "overlay"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test chat_test.mjs: %v\n%s", err, out)
	}
}
