package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// OpenCodeVersion runs `opencode --version` and returns the trimmed
// stdout. opencode 1.x prints just the bare version (e.g. "1.14.48").
// Callers gate on it via OpencodeVersionAtLeast (the ACP floor) or
// report it in the software manifest.
//
// The subprocess inherits clank-host's environment (HOME etc.);
// reading the version doesn't touch session storage.
func OpenCodeVersion(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "opencode", "--version")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		stderr := ""
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		return "", fmt.Errorf("opencode --version: %w: %s", err, stderr)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("opencode --version: empty output")
	}
	return v, nil
}
