package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// PinnedHermesVersion is the verified-surface floor for `hermes acp` —
// the hermes-agent version the ACP integration was verified against
// (protocol v1, session list/load/resume, SessionModeState modes,
// request_permission). The ACP manager refuses older installs with an
// upgrade hint. Like opencode, hermes is the user's own install; clank
// pins nothing, it only gates.
const PinnedHermesVersion = "0.19.0"

// HermesVersionAtLeast reports whether version v is >= floor. Used by
// the ACP path to gate `hermes acp` on the verified-surface floor.
func HermesVersionAtLeast(v, floor string) (bool, error) {
	return versionAtLeast(v, floor)
}

// HermesACPVersion runs `hermes acp --version` and returns the trimmed
// stdout — the bare version string (e.g. "0.19.0"), unlike the
// multi-line `hermes --version`. The subcommand existing at all also
// proves the install carries the acp extra's entrypoint.
func HermesACPVersion(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "hermes", "acp", "--version")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		stderr := ""
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		return "", fmt.Errorf("hermes acp --version: %w: %s", err, stderr)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("hermes acp --version: empty output")
	}
	return v, nil
}
