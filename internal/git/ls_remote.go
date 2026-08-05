package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// LsRemoteDefaultBranch asks a remote for the branch its HEAD points at,
// deliberately without credentials: `-c credential.helper=` resets any
// configured helpers and GIT_TERMINAL_PROMPT=0 fails fast instead of
// prompting, so a successful answer proves the URL is publicly clonable.
// Unlike GitHub's REST API, anonymous git smart-HTTP is not subject to the
// tiny unauthenticated per-IP rate limit.
func LsRemoteDefaultBranch(ctx context.Context, url string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-c", "credential.helper=", "ls-remote", "--symref", "--", url, "HEAD")
	cmd.Env = envWithTerminalPromptDisabled()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git ls-remote: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		// The symref advertisement looks like "ref: refs/heads/main\tHEAD".
		rest, isSymref := strings.CutPrefix(line, "ref: refs/heads/")
		if !isSymref {
			continue
		}
		if branch, _, found := strings.Cut(rest, "\t"); found && branch != "" {
			return branch, nil
		}
	}
	return "", fmt.Errorf("git ls-remote: %s advertised no default branch", url)
}

// envWithTerminalPromptDisabled returns os.Environ() with GIT_TERMINAL_PROMPT
// forced to 0. A plain append can't override an inherited value: getenv
// resolves duplicate keys to the first match, so a stray GIT_TERMINAL_PROMPT
// already in the process environment would otherwise win over ours.
func envWithTerminalPromptDisabled() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") {
			filtered = append(filtered, kv)
		}
	}
	return append(filtered, "GIT_TERMINAL_PROMPT=0")
}
