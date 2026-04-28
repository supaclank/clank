// Package sync implements hub-to-hub git synchronization. It owns the
// laptop-side push agent that bundles unpushed commits and ships them
// to a remote hub, and the cloud-side mirror manager that receives
// bundles, unpacks them into a bare git repo, and serves the result
// over smart-HTTP so sandboxes can clone it.
package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MirrorRoot manages a directory holding one bare git mirror per
// synced repo, keyed by stable repo keys (agent.RepoKey hashes). The
// cloud hub uses this to receive synced data from laptop hubs.
type MirrorRoot struct {
	dir string // e.g. ~/.clank/sync
}

// NewMirrorRoot creates the root directory if needed. dir is typically
// filepath.Join(config.Dir(), "sync").
func NewMirrorRoot(dir string) (*MirrorRoot, error) {
	if dir == "" {
		return nil, fmt.Errorf("mirror root: dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir mirror root: %w", err)
	}
	return &MirrorRoot{dir: dir}, nil
}

// Dir returns the mirror root directory.
func (m *MirrorRoot) Dir() string { return m.dir }

// Mirror returns a handle to the mirror for repoKey. Creates the bare
// repo on first use; idempotent. repoKey must be a sanitized stable
// identifier — the host plane's agent.RepoKey hashed; this package
// does not enforce that.
func (m *MirrorRoot) Mirror(repoKey string) (*RepoMirror, error) {
	if !safeRepoKey(repoKey) {
		return nil, fmt.Errorf("mirror: invalid repo_key %q", repoKey)
	}
	repoDir := filepath.Join(m.dir, repoKey, "repo.git")
	incomingDir := filepath.Join(m.dir, repoKey, "incoming")
	if err := os.MkdirAll(incomingDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir incoming dir: %w", err)
	}
	rm := &RepoMirror{
		repoKey:     repoKey,
		dir:         repoDir,
		incomingDir: incomingDir,
	}
	if err := rm.ensureBareRepo(); err != nil {
		return nil, err
	}
	return rm, nil
}

// safeRepoKey rejects keys that would let a caller traverse the mirror
// root via the filesystem layout. The repo_key is hub-controlled (it
// comes out of agent.RepoKey-derived hashing on the wire), but treating
// it as untrusted at this boundary keeps the assertion local to this
// package.
func safeRepoKey(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, `/\` + "\x00") {
		return false
	}
	return true
}

// RepoMirror is one repo's bare git mirror on disk.
type RepoMirror struct {
	repoKey     string
	dir         string // bare repo directory (..../repo.git)
	incomingDir string // staging dir for in-flight bundle uploads
}

// Path returns the bare repo directory.
func (r *RepoMirror) Path() string { return r.dir }

// ensureBareRepo runs `git init --bare` if the directory is empty, and
// is a no-op once initialized. Safe to call repeatedly.
func (r *RepoMirror) ensureBareRepo() error {
	headPath := filepath.Join(r.dir, "HEAD")
	if _, err := os.Stat(headPath); err == nil {
		return nil
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir bare repo: %w", err)
	}
	cmd := exec.Command("git", "init", "--bare", r.dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git init --bare: %w: %s", err, out)
	}
	return nil
}

// Unbundle streams a git bundle into the mirror, writing the bundle's
// branch tip to refs/heads/<branch>. The bundle is spooled to a temp
// file in the incoming directory because `git bundle unbundle` requires
// a real path. The temp file is removed whether the operation succeeds
// or fails so the staging directory does not accumulate cruft.
//
// Returns the new tip SHA from refs/heads/<branch> on success.
func (r *RepoMirror) Unbundle(ctx context.Context, branch string, body io.Reader) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("unbundle: branch is required")
	}
	if !safeBranchName(branch) {
		return "", fmt.Errorf("unbundle: invalid branch name %q", branch)
	}

	tmp, err := os.CreateTemp(r.incomingDir, "bundle-*.bundle")
	if err != nil {
		return "", fmt.Errorf("create temp bundle: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write bundle to temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp bundle: %w", err)
	}

	// `git bundle verify` first — surfaces "missing prerequisites" before
	// we let unbundle import a partial pack.
	if out, err := r.runGit(ctx, "bundle", "verify", tmpPath); err != nil {
		return "", fmt.Errorf("bundle verify failed: %w: %s", err, out)
	}

	// `git bundle unbundle` only imports objects — it does NOT update
	// refs, despite the optional refname argument (which filters which
	// refs are printed back). To get the new tip into refs/heads/<branch>
	// we look up what the bundle claims for the branch via list-heads,
	// then update-ref ourselves. This also lets us accept bundles that
	// recorded the ref under either "<branch>" or "refs/heads/<branch>".
	bundleSHA, err := r.bundleHeadSHA(ctx, tmpPath, branch)
	if err != nil {
		return "", err
	}
	if bundleSHA == "" {
		return "", fmt.Errorf("bundle for %q has no matching ref", branch)
	}

	if out, err := r.runGit(ctx, "bundle", "unbundle", tmpPath); err != nil {
		return "", fmt.Errorf("bundle unbundle: %w: %s", err, out)
	}

	ref := "refs/heads/" + branch
	if out, err := r.runGit(ctx, "update-ref", ref, bundleSHA); err != nil {
		return "", fmt.Errorf("update-ref %s: %w: %s", ref, err, out)
	}

	// `git init --bare` sets HEAD to refs/heads/main even when no such
	// ref exists. A clone from a mirror whose HEAD points at a missing
	// ref produces an empty working tree (git prints a warning and
	// exits 0). Make the first received branch the default so clones
	// from the mirror have a sensible checkout. Subsequent bundles for
	// other branches don't overwrite — the user's workflow already
	// has a primary branch by the time the second one shows up.
	if err := r.ensureHEADTargetExists(ctx, ref); err != nil {
		return "", err
	}

	tip, err := r.RefHead(ctx, ref)
	if err != nil {
		return "", err
	}
	return tip, nil
}

// ensureHEADTargetExists points HEAD at ref iff HEAD currently
// references something that does not exist (the post-`git init --bare`
// state, where HEAD → refs/heads/main even though no ref has ever
// been created). No-op if HEAD already resolves.
func (r *RepoMirror) ensureHEADTargetExists(ctx context.Context, ref string) error {
	out, err := r.runGit(ctx, "symbolic-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("symbolic-ref HEAD: %w: %s", err, out)
	}
	currentHEAD := strings.TrimSpace(string(out))
	if _, err := r.runGit(ctx, "rev-parse", "--verify", "--quiet", currentHEAD); err == nil {
		return nil // HEAD already resolves; leave it alone
	}
	if out, err := r.runGit(ctx, "symbolic-ref", "HEAD", ref); err != nil {
		return fmt.Errorf("symbolic-ref HEAD %s: %w: %s", ref, err, out)
	}
	return nil
}

// bundleHeadSHA inspects a bundle and returns the SHA recorded for the
// given branch, accepting either a fully-qualified refs/heads/<name> form
// or the bare <name> form (some `git bundle create` invocations write
// the bare form). Returns empty string when no match is found.
func (r *RepoMirror) bundleHeadSHA(ctx context.Context, bundlePath, branch string) (string, error) {
	out, err := r.runGit(ctx, "bundle", "list-heads", bundlePath)
	if err != nil {
		return "", fmt.Errorf("bundle list-heads: %w: %s", err, out)
	}
	wantFull := "refs/heads/" + branch
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sha, name := fields[0], fields[1]
		if name == wantFull || name == branch {
			return sha, nil
		}
	}
	return "", nil
}

// RefHead returns the SHA pointed to by ref (e.g. "refs/heads/main"),
// or empty string if the ref does not exist.
func (r *RepoMirror) RefHead(ctx context.Context, ref string) (string, error) {
	out, err := r.runGit(ctx, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		// rev-parse --verify --quiet exits non-zero with no output when
		// the ref is missing. Distinguish that from a real failure by
		// looking at the output.
		if strings.TrimSpace(string(out)) == "" {
			return "", nil
		}
		return "", fmt.Errorf("rev-parse %s: %w: %s", ref, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// runGit shells out to git -C <bare repo>. Returns combined stdout+stderr
// so callers can include it in error messages.
func (r *RepoMirror) runGit(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"-C", r.dir}, args...)
	return exec.CommandContext(ctx, "git", full...).CombinedOutput()
}

// safeBranchName rejects branch names that contain characters git would
// also reject for refs (or that would let a caller escape the ref
// namespace). git itself enforces these via check-ref-format on update,
// but rejecting at the boundary gives a clearer error than letting git
// barf out of a child process.
func safeBranchName(s string) bool {
	if s == "" || s == "@" {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, "//") {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
		switch c {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return false
		}
	}
	return true
}
