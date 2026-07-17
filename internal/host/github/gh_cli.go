package github

// The machine's own `gh` CLI as a token source. `gh auth token` is
// gh's public handoff command: it resolves $GH_TOKEN, the OS keychain,
// and hosts.yml exactly the way gh itself would, so clank never reads
// gh's internals — if gh works in the user's terminal, the same
// credential works here.

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// ghCLITimeout bounds the `gh auth token` probe. The command reads
// local config/keychain state — anything slower is a hung helper, not
// a slow network.
const ghCLITimeout = 3 * time.Second

// ghCLIToken returns the token the machine's gh CLI would use, or ""
// when gh is absent, logged out (gh exits non-zero), or prints
// something that can't be a token. For the fallback chain those are
// all ordinary "no credential here" states, not faults to surface.
func ghCLIToken() string {
	path, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), ghCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "token")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	token := strings.TrimSpace(out.String())
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return ""
	}
	return token
}
