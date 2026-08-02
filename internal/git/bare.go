package git

// Bare canonical repositories — the repo-first storage model's shared
// clone. One bare, BLOBLESS canonical per repo lives at
// ~/work/repos/<slug>/repo.git; every ~/work/<worktreeID> is a linked
// worktree of it (`git worktree add`), sharing refs and objects. Blobs
// download lazily on checkout via the repo-configured credential helper
// (internal/host/github/git_credential.go), so a canonical costs ~the
// commit/tree skeleton, not the full blob history.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// allHeadsRefspec mirrors every remote branch into the remote-tracking
// namespace — the fetch refspec a non-bare clone gets implicitly but a
// bare clone doesn't.
const allHeadsRefspec = "+refs/heads/*:refs/remotes/origin/*"

// CloneBare clones url into gitDir (a *.git path that must not exist) as a
// bare, BLOBLESS (--filter=blob:none), single-branch repository, keeping
// origin. branch selects which branch's ref to fetch (empty = the remote's
// default). After cloning it repairs the two things `git clone --bare`
// leaves unset for our model:
//
//   - remote.origin.fetch = +refs/heads/*:refs/remotes/origin/* (bare
//     clones get NO fetch refspec, so fetches would update nothing)
//   - credential.helper = credentialHelper, so lazy blob fetches and any
//     later authenticated operation can find a token on their own
//
// token auths the clone itself like CloneShallowKeepRemote (env + inline
// helper; never argv or config). credentialHelper is the persistent value
// (see github.GitCredentialHelperValue); empty skips the config (tests,
// public-only setups).
//
// --single-branch keeps refs/heads/* = "branches clank manages": the
// canonical starts with just the imported branch and grows one local ref
// per loaded/forked branch, which is exactly the set the repo overview
// reports.
func CloneBare(ctx context.Context, url, gitDir, token, branch, credentialHelper string) error {
	args := []string{}
	if token != "" {
		args = append(args,
			"-c", "credential.helper=",
			"-c", "credential.helper="+cloneCredentialHelper,
		)
	}
	args = append(args, "clone", "--bare", "--filter=blob:none", "--single-branch")
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", url, gitDir)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if token != "" {
		cmd.Env = append(cmd.Env, cloneTokenEnv+"="+token)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone --bare: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}
	if err := SetLocalConfig(gitDir, "remote.origin.fetch", allHeadsRefspec); err != nil {
		return err
	}
	if credentialHelper != "" {
		if err := SetLocalConfig(gitDir, "credential.helper", credentialHelper); err != nil {
			return err
		}
	}
	// A bare clone creates NO remote-tracking refs. Seed the cloned
	// branch's tracking ref from its tip — truthful (the local tip IS
	// origin's tip at clone instant) and zero-network — so ahead/behind
	// has a comparison point from the first moment instead of only after
	// the first fetch. An EMPTY origin has an unborn HEAD with no tip to
	// seed; skip quietly (there's nothing to compare against anyway).
	head, err := HeadBranch(gitDir)
	if err != nil {
		return fmt.Errorf("resolve cloned head: %w", err)
	}
	if sha, err := RevParse(gitDir, "refs/heads/"+head); err == nil {
		if _, err := gitCmd(gitDir, "update-ref", "refs/remotes/origin/"+head, sha); err != nil {
			return fmt.Errorf("seed tracking ref: %w", err)
		}
	}
	return nil
}

// InitBare creates an empty bare repository at gitDir with HEAD pointing
// at defaultBranch — the greenfield canonical, which gets its first
// commit via a filesystem push from the scaffold's temp checkout.
func InitBare(ctx context.Context, gitDir, defaultBranch string) error {
	cmd := exec.CommandContext(ctx, "git", "init", "--bare", "-b", defaultBranch, gitDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git init --bare %s: %s (%w)", gitDir, strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// HeadBranch returns the branch HEAD symbolically points at in the repo
// at dir. This is the canonical's default branch — DefaultBranch's
// main/master probing is wrong for single-branch clones of a repo whose
// default is neither. Errors on a detached HEAD.
func HeadBranch(dir string) (string, error) {
	out, err := gitCmd(dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("head branch: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// BranchTip is one refs/heads entry: the branch, its tip SHA, and the tip
// commit's committer time.
type BranchTip struct {
	Branch      string
	SHA         string
	CommittedAt time.Time
}

// LocalBranchTips returns every refs/heads/* tip via a single
// for-each-ref invocation — the repo overview's git half without N×
// `git log`. Ordered most recently committed first.
func LocalBranchTips(dir string) ([]BranchTip, error) {
	out, err := gitCmd(dir, "for-each-ref",
		"--sort=-committerdate",
		"--format=%(refname:short)%00%(objectname)%00%(committerdate:iso-strict)",
		"refs/heads")
	if err != nil {
		return nil, fmt.Errorf("list branch tips: %w", err)
	}
	var tips []BranchTip
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("list branch tips: malformed line %q", line)
		}
		committedAt, err := time.Parse(time.RFC3339, parts[2])
		if err != nil {
			return nil, fmt.Errorf("list branch tips: parse date %q: %w", parts[2], err)
		}
		tips = append(tips, BranchTip{Branch: parts[0], SHA: parts[1], CommittedAt: committedAt})
	}
	return tips, nil
}

// RemoteTrackingBranchExists reports whether refs/remotes/<remote>/<branch>
// resolves in dir. (BranchExists checks refs/heads only.) Used to guard
// ahead/behind computations that need a tracking ref to compare against.
func RemoteTrackingBranchExists(dir, remote, branch string) (bool, error) {
	_, err := gitCmd(dir, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+branch)
	if err == nil {
		return true, nil
	}
	// rev-parse --verify --quiet exits 1 for a missing ref with no
	// stderr; that's the "no" answer, not a fault.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// GetLocalConfig reads a repository-local git config value from the repo
// at dir. Returns ("", nil) when the key is unset — the read-side
// counterpart of SetLocalConfig.
func GetLocalConfig(dir, key string) (string, error) {
	out, err := gitCmd(dir, "config", "--local", "--get", key)
	if err != nil {
		// `git config --get` exits 1 when the key is simply absent.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("git config --get %s: %w", key, err)
	}
	return strings.TrimSpace(out), nil
}

// PruneWorktrees runs `git worktree prune` in the repo at dir, dropping
// bookkeeping for worktree dirs that no longer exist on disk (manual rm,
// failed adds).
func PruneWorktrees(dir string) error {
	if _, err := gitCmd(dir, "worktree", "prune"); err != nil {
		return fmt.Errorf("git worktree prune: %w", err)
	}
	return nil
}

// CommonDir returns the repo's shared git directory (absolute): a
// non-bare repo's `.git`, a linked worktree's parent repo git dir, or a
// bare repo's own path. The primitive under MainWorktreeRoot, exposed
// for callers that want the git dir itself (e.g. resolving which
// canonical a linked worktree belongs to).
func CommonDir(dir string) (string, error) {
	out, err := gitCmd(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("git common dir: %w", err)
	}
	common := strings.TrimSpace(out)
	if common == "" {
		return "", fmt.Errorf("git common-dir returned empty path for %q", dir)
	}
	return common, nil
}
