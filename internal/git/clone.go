package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Clone shallow-clones the repository at url into dir. dir must not
// already exist — git creates it. Only the tip commit is fetched
// (--depth 1); callers that want a template's files but not its history
// pair this with removing dir/.git and a fresh Init.
func Clone(ctx context.Context, url, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--", url, dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// cloneTokenEnv carries the HTTPS auth token into the git subprocess for
// CloneShallowKeepRemote. Passing it via the environment (not argv) keeps
// it out of `ps` output; the inline credential helper reads it at runtime.
const cloneTokenEnv = "CLANK_GH_TOKEN"

// cloneCredentialHelper is an inline git credential helper that answers
// HTTPS auth prompts with the token from cloneTokenEnv. GitHub accepts any
// username when the password is a token, so "x-access-token" is a stable
// placeholder. Passed via `git -c`, it never lands in the cloned repo's
// .git/config — so the stored remote URL stays clean.
const cloneCredentialHelper = `!f() { echo username=x-access-token; echo "password=$` + cloneTokenEnv + `"; }; f`

// CloneShallowKeepRemote shallow-clones url into dir (--depth 1) keeping
// the .git directory and the origin remote — for importing a user's
// existing repo, where history depth doesn't matter but the link back to
// the remote does. token authenticates HTTPS clones of private repos; pass
// "" for public repos. branch checks out a specific branch (empty clones
// the remote's default). dir must not already exist.
//
// The token reaches git through cloneTokenEnv + an inline credential
// helper rather than the URL, so it appears neither in argv nor in the
// resulting .git/config. The leading empty credential.helper resets any
// system/global helper so it can't shadow ours.
func CloneShallowKeepRemote(ctx context.Context, url, dir, token, branch string) error {
	args := []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=" + cloneCredentialHelper,
		"clone", "--depth", "1",
	}
	// Clone a single branch when requested (import branch selection); an
	// empty branch clones the remote's default. The caller validates that
	// branch doesn't begin with "-" so it can't be misread as a flag; the
	// "--" separator still guards the positional url/dir.
	if branch != "" {
		args = append(args, "--branch", branch, "--single-branch")
	}
	args = append(args, "--", url, dir)
	cmd := exec.CommandContext(ctx, "git", args...)
	// GIT_TERMINAL_PROMPT=0 fails fast on bad/missing auth instead of
	// blocking on an interactive username/password prompt.
	cmd.Env = append(os.Environ(), cloneTokenEnv+"="+token, "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// Init initializes a new git repository at dir with defaultBranch as the
// initial branch (git init -b <branch>). dir is created if needed.
func Init(ctx context.Context, dir, defaultBranch string) error {
	cmd := exec.CommandContext(ctx, "git", "init", "-b", defaultBranch, dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git init %s: %s (%w)", dir, strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// SetLocalConfig sets a repository-local git config value (git config
// <key> <value>) in the repo at dir. Used to give a freshly-initialised
// project a committer identity so the initial commit doesn't depend on
// global git config being present on the host.
func SetLocalConfig(dir, key, value string) error {
	_, err := gitCmd(dir, "config", key, value)
	if err != nil {
		return fmt.Errorf("git config %s: %w", key, err)
	}
	return nil
}
