package clankcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/pkg/sync/checkpoint"
)

// parityResult is the decision input for `clank push`/`pull` flows.
// Filled by checkParity from a local Snapshot + the remote's
// latest-checkpoint metadata.
type parityResult struct {
	OwnerKind      string // "local" | "remote"; "" if the remote has no row for this worktree
	InSync         bool   // true when all 4 content SHAs match the remote's latest checkpoint
	HasCheckpoint  bool   // false when the remote knows the worktree but no checkpoint has been pushed yet
	RemoteHead     string // remote's head_commit (or "" when HasCheckpoint == false); for messaging
	LocalHead      string // local's head_commit; for messaging
	RemoteNotFound bool   // true when the worktree row doesn't exist on the remote (e.g. fresh repo)
}

// checkParity compares a local checkpoint snapshot against the remote
// sync server's view of the same worktree. Single GET round-trip;
// builds nothing locally beyond a Snapshot (which is git plumbing,
// no bundle I/O).
//
// Returns a parityResult and a non-nil error only on hard failures
// (network down with no cached state, malformed response). A remote
// that says "no such worktree" returns parity{RemoteNotFound: true},
// not an error.
func checkParity(ctx context.Context, dc *daemonclient.Client, worktreeID string, snap *checkpoint.Snapshot) (parityResult, error) {
	if worktreeID == "" {
		return parityResult{}, errors.New("parity: worktreeID required")
	}
	if snap == nil {
		return parityResult{}, errors.New("parity: snapshot required")
	}
	wt, err := dc.GetWorktree(ctx, worktreeID)
	if err != nil {
		if errors.Is(err, daemonclient.ErrWorktreeNotFound) {
			return parityResult{RemoteNotFound: true, LocalHead: snap.HeadCommit}, nil
		}
		return parityResult{}, fmt.Errorf("get worktree: %w", err)
	}

	res := parityResult{
		OwnerKind: wt.OwnerKind,
		LocalHead: snap.HeadCommit,
	}
	if wt.LatestCheckpointMetadata == nil {
		// Worktree exists on remote but no checkpoint pushed yet. The
		// laptop is implicitly "ahead" — any push is a first push.
		// InSync stays false; callers see HasCheckpoint=false and
		// branch on it.
		return res, nil
	}
	res.HasCheckpoint = true
	res.RemoteHead = wt.LatestCheckpointMetadata.HeadCommit

	res.InSync = wt.LatestCheckpointMetadata.HeadCommit == snap.HeadCommit &&
		wt.LatestCheckpointMetadata.HeadRef == snap.HeadRef &&
		wt.LatestCheckpointMetadata.IndexTree == snap.IndexTree &&
		wt.LatestCheckpointMetadata.WorktreeTree == snap.WorktreeTree
	return res, nil
}

// snapshotRepo is a small helper that resolves a repo path to a
// Snapshot. Wraps the underlying Builder so callers don't have to
// reach into pkg/sync/checkpoint directly.
func snapshotRepo(ctx context.Context, repoPath string) (*checkpoint.Snapshot, error) {
	return checkpoint.NewBuilder(repoPath, "laptop").Snapshot(ctx)
}

// isGitRepo reports whether repoPath is an initialized git working
// tree (has a .git entry). Returns (false, nil) for both
// "path doesn't exist" and "path exists but no .git" — both are
// "skip the snapshot/parity fast-path" from pull's perspective.
func isGitRepo(repoPath string) (bool, error) {
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
