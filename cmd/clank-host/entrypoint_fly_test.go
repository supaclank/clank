package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestEntrypointFlyRequiresAuthToken pins the fail-fast contract for the Fly
// image entrypoint: unlike entrypoint.sh's laptop-local mode, the Fly image
// is always cloud-exposed, so a missing bearer must abort before ever
// reaching clank-host rather than falling back to unauthenticated (CR
// discussion_r3565279772).
func TestEntrypointFlyRequiresAuthToken(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sh", "entrypoint-fly.sh")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure with CLANK_HOST_AUTH_TOKEN unset, got success: %s", out)
	}
	if !strings.Contains(string(out), "CLANK_HOST_AUTH_TOKEN") {
		t.Fatalf("expected error to mention CLANK_HOST_AUTH_TOKEN, got: %s", out)
	}
}
