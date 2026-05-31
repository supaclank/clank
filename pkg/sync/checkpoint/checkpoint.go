// Package checkpoint constructs and restores worktree checkpoints in
// the form of two git bundles plus a manifest:
//
//   - headCommit bundle  — `git bundle` covering the current HEAD's
//     reachable history. Heavy but rarely changes.
//   - uncommitted bundle — a synthetic commit produced via
//     `git commit-tree` against a tree that includes uncommitted /
//     staged / untracked work. Light, often changes.
//
// A Manifest captures HEAD SHA, HEAD ref, indexTree SHA, worktreeTree
// SHA, and the synthetic uncommitted commit SHA. Restoring (Apply)
// reproduces the exact pre-checkpoint working state, including
// untracked files and staged-but-uncommitted changes.
//
// .gitignore'd files are NOT included in the worktreeTree by default;
// they would balloon every checkpoint with build artifacts. A future
// configuration knob can opt in to including ignored files (e.g. for
// .env passthrough).
package checkpoint

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ManifestVersion is bumped when the on-disk manifest schema changes
// in a non-backwards-compatible way. Apply refuses unknown versions.
const ManifestVersion = 1

// Manifest is the per-checkpoint metadata blob, stored alongside the
// two bundles in object storage.
//
// OriginRemoteURL is a v1-backwards-compatible extension: pre-fix
// manifests have an empty value (omitempty), which Apply treats as
// "do nothing with origin." New manifests carry the laptop's
// `git remote get-url origin` so sprite-side worktrees aren't
// origin-less after Apply (was the root cause of the PR-creation
// "no origin" / wrong-repo classes of failures).
type Manifest struct {
	Version           int       `json:"version"`
	CheckpointID      string    `json:"checkpoint_id"`
	HeadCommit        string    `json:"head_commit"`
	HeadRef           string    `json:"head_ref"`
	IndexTree         string    `json:"index_tree"`
	WorktreeTree      string    `json:"worktree_tree"`
	UncommittedCommit string    `json:"uncommitted_commit"`
	CreatedAt         time.Time `json:"created_at"`
	CreatedBy         string    `json:"created_by"`
	OriginRemoteURL   string    `json:"origin_remote_url,omitempty"`
}

// Marshal serializes a Manifest to canonical JSON. Stable enough to
// HMAC-sign for tamper detection (P3); the JSON encoder produces
// deterministic output for our struct shape because Go's encoder
// preserves field declaration order and we have no maps.
func (m *Manifest) Marshal() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// UnmarshalManifest parses a Manifest blob and rejects unknown
// versions.
func UnmarshalManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("checkpoint: parse manifest: %w", err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("checkpoint: unsupported manifest version %d (want %d)", m.Version, ManifestVersion)
	}
	return &m, nil
}

// Result is the output of Builder.Build. The bundle files live under
// os.TempDir; callers MUST invoke Cleanup() when done.
type Result struct {
	Manifest          *Manifest
	HeadCommitBundle  string
	UncommittedBundle string
}

// Cleanup removes the temp bundle files. Safe to call multiple times.
func (r *Result) Cleanup() {
	if r == nil {
		return
	}
	if r.HeadCommitBundle != "" {
		_ = os.Remove(r.HeadCommitBundle)
		r.HeadCommitBundle = ""
	}
	if r.UncommittedBundle != "" {
		_ = os.Remove(r.UncommittedBundle)
		r.UncommittedBundle = ""
	}
}

// Builder builds checkpoints from a git working directory.
type Builder struct {
	repoPath  string
	createdBy string
}

// NewBuilder constructs a Builder rooted at repoPath. createdBy is
// stamped into the manifest (typically "laptop:<device_id>" or
// "sprite:<host_id>").
func NewBuilder(repoPath, createdBy string) *Builder {
	return &Builder{repoPath: repoPath, createdBy: createdBy}
}

// Snapshot is the content-addressed view of the working tree without
// any bundling work. Cheaper than Build: 4 git plumbing calls, no
// commit-tree synthesis, no `git bundle create`. Used by divergence
// detection in push/pull (`clank status`, `clank push` idempotency)
// to decide "is local already in sync with remote" before committing
// to a full bundle upload.
type Snapshot struct {
	HeadCommit   string
	HeadRef      string
	IndexTree    string
	WorktreeTree string
}

// Snapshot computes the 4 content SHAs that uniquely identify the
// working tree's state. No filesystem writes (other than git's own
// object hashing).
func (b *Builder) Snapshot(ctx context.Context) (*Snapshot, error) {
	return b.snapshot(ctx)
}

// snapshot is the shared SHA-capture routine. Both Snapshot (parity
// fast-path) and Build (full checkpoint) start from these 4 fields;
// keeping a single implementation rules out silent divergence — if
// the two paths emit different SHAs for the same working tree, the
// parity check breaks idempotency without anyone noticing.
func (b *Builder) snapshot(ctx context.Context) (*Snapshot, error) {
	headCommit, err := b.gitOutput(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	headCommit = strings.TrimSpace(headCommit)

	headRef := ""
	if out, err := b.gitOutput(ctx, nil, "symbolic-ref", "--short", "HEAD"); err == nil {
		headRef = strings.TrimSpace(out)
	}

	indexTreeOut, err := b.gitOutput(ctx, nil, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("write-tree (index): %w", err)
	}
	indexTree := strings.TrimSpace(indexTreeOut)

	worktreeTree, err := b.captureWorktreeTree(ctx, headCommit)
	if err != nil {
		return nil, fmt.Errorf("capture worktreeTree: %w", err)
	}

	return &Snapshot{
		HeadCommit:   headCommit,
		HeadRef:      headRef,
		IndexTree:    indexTree,
		WorktreeTree: worktreeTree,
	}, nil
}

// Build constructs a full checkpoint: the uncommitted bundle + manifest
// plus a FULL head bundle (all HEAD history). Equivalent to
// BuildUncommitted followed by a full BuildHeadBundle. Callers that
// always want both bundles use this; the laptop's content-addressed
// push path uses the two methods separately so it can skip the head
// bundle when the server already has that HEAD.
func (b *Builder) Build(ctx context.Context, checkpointID string) (*Result, error) {
	res, err := b.BuildUncommitted(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	headBundle, err := b.BuildHeadBundle(ctx, checkpointID, res.Manifest.HeadCommit, "")
	if err != nil {
		res.Cleanup()
		return nil, err
	}
	res.HeadCommitBundle = headBundle
	return res, nil
}

// BuildUncommitted builds the uncommitted bundle (worktree/index/
// untracked delta on top of HEAD) and the manifest, leaving the head
// bundle to a separate BuildHeadBundle call. Result.HeadCommitBundle is
// "" — the caller fills it after deciding whether the head bundle is
// needed (content-addressed dedup). The uncommitted bundle is bounded by
// the head COMMIT (`^headCommit`), so it needs no head bundle/ref.
func (b *Builder) BuildUncommitted(ctx context.Context, checkpointID string) (*Result, error) {
	if checkpointID == "" {
		return nil, errors.New("checkpoint: checkpointID is required")
	}

	snap, err := b.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	headCommit := snap.HeadCommit

	// Capture origin's URL so Apply can re-create the remote on the
	// destination. Error-tolerant: pre-clank-push laptop repos and
	// fresh worktrees may have no origin, which is fine — empty
	// string flows through omitempty and Apply skips the restore.
	originURL, originErr := b.gitOutput(ctx, nil, "config", "--get", "remote.origin.url")
	if originErr != nil {
		// `git config --get` exits 1 when the key isn't set. We swallow
		// any error here (including "not a git repo") because origin
		// is genuinely optional metadata, not a correctness boundary.
		originURL = ""
	}
	originURL = strings.TrimSpace(originURL)

	commitMsg := "clank checkpoint " + checkpointID

	// Synthesize an "index" commit so the indexTree object is reachable
	// from the bundle. Without this, restoring the index via
	// `git read-tree <indexTree>` fails because the tree object isn't in
	// the destination's .git/objects/. Then make the worktree commit's
	// second parent point at the index commit so a single bundle covers
	// both trees and their blobs.
	indexCommit, err := b.gitOutput(ctx, nil, "commit-tree", snap.IndexTree, "-p", headCommit, "-m", commitMsg+" (index)")
	if err != nil {
		return nil, fmt.Errorf("commit-tree (index): %w", err)
	}
	indexCommit = strings.TrimSpace(indexCommit)

	incrCommit, err := b.gitOutput(ctx, nil, "commit-tree", snap.WorktreeTree, "-p", headCommit, "-p", indexCommit, "-m", commitMsg+" (worktree)")
	if err != nil {
		return nil, fmt.Errorf("commit-tree (worktree): %w", err)
	}
	incrCommit = strings.TrimSpace(incrCommit)

	incrRefName := tempRefUncommitted(checkpointID)
	if err := b.gitRun(ctx, nil, "update-ref", incrRefName, incrCommit); err != nil {
		return nil, fmt.Errorf("update-ref %s: %w", incrRefName, err)
	}
	defer b.deleteRef(ctx, incrRefName)

	incrBundle, err := tempBundleFile("clank-uncommitted-")
	if err != nil {
		return nil, err
	}

	res := &Result{
		Manifest: &Manifest{
			Version:           ManifestVersion,
			CheckpointID:      checkpointID,
			HeadCommit:        headCommit,
			HeadRef:           snap.HeadRef,
			IndexTree:         snap.IndexTree,
			WorktreeTree:      snap.WorktreeTree,
			UncommittedCommit: incrCommit,
			CreatedAt:         time.Now().UTC(),
			CreatedBy:         b.createdBy,
			OriginRemoteURL:   originURL,
		},
		UncommittedBundle: incrBundle,
	}

	if err := b.gitRun(ctx, nil, "bundle", "create", incrBundle, incrRefName, "^"+headCommit); err != nil {
		res.Cleanup()
		return nil, fmt.Errorf("bundle uncommitted: %w", err)
	}
	return res, nil
}

// BuildHeadBundle bundles HEAD's committed history to a temp file and
// returns its path (the caller removes it). base == "" produces a FULL
// bundle (`git bundle create f HEAD` — all history); a non-empty base
// produces an INCREMENTAL bundle (`… HEAD ^base` — only commits after
// base), whose applier must already have base. The temp ref is created
// and deleted here; the bundle file is self-contained.
func (b *Builder) BuildHeadBundle(ctx context.Context, checkpointID, headCommit, base string) (string, error) {
	headRefName := tempRefHead(checkpointID)
	if err := b.gitRun(ctx, nil, "update-ref", headRefName, headCommit); err != nil {
		return "", fmt.Errorf("update-ref %s: %w", headRefName, err)
	}
	defer b.deleteRef(ctx, headRefName)

	headBundle, err := tempBundleFile("clank-headcommit-")
	if err != nil {
		return "", err
	}
	args := []string{"bundle", "create", headBundle, headRefName}
	if base != "" {
		args = append(args, "^"+base)
	}
	if err := b.gitRun(ctx, nil, args...); err != nil {
		_ = os.Remove(headBundle)
		return "", fmt.Errorf("bundle headCommit: %w", err)
	}
	return headBundle, nil
}

// captureWorktreeTree builds a tree object representing the working
// directory exactly as it stands now (committed + staged + unstaged +
// untracked). Uses a temp index file via GIT_INDEX_FILE so the user's
// real index is untouched.
func (b *Builder) captureWorktreeTree(ctx context.Context, headCommit string) (string, error) {
	tmp, err := os.CreateTemp("", "clank-checkpoint-index-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	env := append(scrubGitEnv(os.Environ()), "GIT_INDEX_FILE="+tmpPath)

	if err := b.gitRun(ctx, env, "read-tree", headCommit); err != nil {
		return "", fmt.Errorf("read-tree HEAD into temp index: %w", err)
	}
	if err := b.gitRun(ctx, env, "add", "-A", "--", "."); err != nil {
		return "", fmt.Errorf("add -A into temp index: %w", err)
	}
	out, err := b.gitOutput(ctx, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write-tree (worktree): %w", err)
	}
	return strings.TrimSpace(out), nil
}

func (b *Builder) deleteRef(ctx context.Context, ref string) {
	_ = b.gitRun(ctx, nil, "update-ref", "-d", ref)
}

// Apply restores a checkpoint to repoPath. repoPath may be a
// non-existent directory (it will be created and `git init`'d), an
// empty directory, or an existing git repo (its state will be
// overwritten to match the checkpoint exactly).
//
// After Apply succeeds:
//   - HEAD points at manifest.HeadCommit
//   - manifest.HeadRef (if non-empty) points at HeadCommit
//   - The index matches manifest.IndexTree
//   - The working tree (incl. untracked-but-checkpointed files)
//     matches manifest.WorktreeTree
//
// Apply does not delete refs that exist in the repo but not in the
// bundle — those are user state and out of scope.
func Apply(ctx context.Context, repoPath string, manifest *Manifest, headCommitBundle, uncommittedBundle io.Reader) error {
	if manifest == nil {
		return errors.New("checkpoint: manifest is nil")
	}
	if manifest.Version != ManifestVersion {
		return fmt.Errorf("checkpoint: unsupported manifest version %d", manifest.Version)
	}

	if err := ensureRepo(ctx, repoPath); err != nil {
		return err
	}

	if err := fetchBundle(ctx, repoPath, headCommitBundle, "headCommit"); err != nil {
		return err
	}
	if err := fetchBundle(ctx, repoPath, uncommittedBundle, "uncommitted"); err != nil {
		return err
	}

	// Position HEAD at the checkpoint's headCommit, on headRef if
	// applicable. Use update-ref --no-deref to overwrite HEAD even if
	// the repo had a different branch checked out.
	if manifest.HeadRef != "" {
		branchRef := "refs/heads/" + manifest.HeadRef
		if err := gitRunIn(ctx, repoPath, nil, "update-ref", branchRef, manifest.HeadCommit); err != nil {
			return fmt.Errorf("update-ref %s: %w", branchRef, err)
		}
		if err := gitRunIn(ctx, repoPath, nil, "symbolic-ref", "HEAD", branchRef); err != nil {
			return fmt.Errorf("symbolic-ref HEAD: %w", err)
		}
	} else {
		if err := gitRunIn(ctx, repoPath, nil, "update-ref", "--no-deref", "HEAD", manifest.HeadCommit); err != nil {
			return fmt.Errorf("update-ref HEAD: %w", err)
		}
	}

	// Drop pre-existing untracked files so the post-Apply state really
	// matches the manifest. read-tree --reset -u only touches tracked
	// paths; without this clean step, untracked files left over from a
	// prior session survive the restore.
	if err := gitRunIn(ctx, repoPath, nil, "clean", "-fd"); err != nil {
		return fmt.Errorf("clean stale untracked: %w", err)
	}
	// Restore working tree from worktreeTree first (this also moves the
	// index there). Then restore the index from indexTree.
	if err := gitRunIn(ctx, repoPath, nil, "read-tree", "--reset", "-u", manifest.WorktreeTree); err != nil {
		return fmt.Errorf("read-tree -u worktreeTree: %w", err)
	}
	if err := gitRunIn(ctx, repoPath, nil, "read-tree", manifest.IndexTree); err != nil {
		return fmt.Errorf("read-tree indexTree: %w", err)
	}

	// Best-effort cleanup of the temp refs the bundle introduced.
	_ = gitRunIn(ctx, repoPath, nil, "update-ref", "-d", tempRefHead(manifest.CheckpointID))
	_ = gitRunIn(ctx, repoPath, nil, "update-ref", "-d", tempRefUncommitted(manifest.CheckpointID))

	// Restore origin's URL when the manifest carries one. Pre-fix
	// manifests have OriginRemoteURL == "" and skip this step
	// cleanly. We use `remote set-url` after `remote add` because
	// `git init` may have left an origin entry from an earlier
	// failed apply, and `remote add origin` fails when origin
	// already exists.
	if manifest.OriginRemoteURL != "" {
		if err := gitRunIn(ctx, repoPath, nil, "remote", "add", "origin", manifest.OriginRemoteURL); err != nil {
			// `remote add` exits 128 when origin already exists;
			// fall through to set-url which is idempotent.
			if err := gitRunIn(ctx, repoPath, nil, "remote", "set-url", "origin", manifest.OriginRemoteURL); err != nil {
				// Don't fail Apply over a remote restore — the
				// checkpoint is fully applied; origin is a nice-to-
				// have. Log and continue.
				_ = err
			}
		}
	}

	return nil
}

func ensureRepo(ctx context.Context, repoPath string) error {
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", repoPath, err)
	}
	gitDir := filepath.Join(repoPath, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", gitDir, err)
	}
	if err := gitRunIn(ctx, repoPath, nil, "init", "--quiet"); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	return nil
}

// fetchBundle pipes a bundle reader into `git bundle unbundle` first
// to materialize objects in .git/objects, then runs `git fetch
// <bundle-file>` from a temp file to update refs. We use a temp file
// because git bundle unbundle reads the bundle data twice (header
// then objects) and a stream Reader can't be seeked.
func fetchBundle(ctx context.Context, repoPath string, bundle io.Reader, label string) error {
	tmp, err := os.CreateTemp("", "clank-apply-"+label+"-*.bundle")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	bw := bufio.NewWriter(tmp)
	if _, err := io.Copy(bw, bundle); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write bundle: %w", err)
	}
	if err := bw.Flush(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush bundle: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close bundle: %w", err)
	}

	if err := gitRunIn(ctx, repoPath, nil, "fetch", "--no-tags", "--update-head-ok", tmpPath, "+refs/*:refs/*"); err != nil {
		return fmt.Errorf("fetch %s bundle: %w", label, err)
	}
	return nil
}

func tempRefHead(checkpointID string) string {
	return "refs/clank-checkpoints/" + checkpointID + "/head"
}

func tempRefUncommitted(checkpointID string) string {
	return "refs/clank-checkpoints/" + checkpointID + "/uncommitted"
}

func tempBundleFile(prefix string) (string, error) {
	f, err := os.CreateTemp("", prefix+"*.bundle")
	if err != nil {
		return "", err
	}
	name := f.Name()
	_ = f.Close()
	return name, nil
}

func (b *Builder) gitRun(ctx context.Context, env []string, args ...string) error {
	return gitRunIn(ctx, b.repoPath, env, args...)
}

func (b *Builder) gitOutput(ctx context.Context, env []string, args ...string) (string, error) {
	return gitOutputIn(ctx, b.repoPath, env, args...)
}

func gitRunIn(ctx context.Context, repoPath string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

// scrubGitEnv strips GIT_* variables that override repository discovery
// from env, so `git -C repoPath` is authoritative even when the parent
// shell has GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE / GIT_CONFIG* set
// (e.g. when `clank push` is invoked from inside another git tool).
// Without scrubbing, captureWorktreeTree silently produces the wrong
// manifest tree.
func scrubGitEnv(env []string) []string {
	bad := map[string]struct{}{
		"GIT_DIR":              {},
		"GIT_WORK_TREE":        {},
		"GIT_OBJECT_DIRECTORY": {},
		"GIT_COMMON_DIR":       {},
		"GIT_NAMESPACE":        {},
		"GIT_INDEX_FILE":       {},
		"GIT_CONFIG":           {},
		"GIT_CONFIG_GLOBAL":    {},
		"GIT_CONFIG_SYSTEM":    {},
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		if _, drop := bad[kv[:eq]]; drop {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func gitOutputIn(ctx context.Context, repoPath string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)), err)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
