package git

import (
	"bytes"
	"context"
	"fmt"
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
