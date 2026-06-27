package host

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/acksell/clank/internal/git"
)

// ResolveStrategy selects how a diverged worktree reconciles with its
// remote.
type ResolveStrategy string

const (
	// ResolveTakeRemote discards local divergence: back up the current
	// HEAD to a recovery ref, then hard-reset the branch to origin/<branch>.
	ResolveTakeRemote ResolveStrategy = "take_remote"
	// ResolveMerge keeps local work and merges origin/<branch> in. A clean
	// merge leaves an unpushed merge commit; a conflicting merge is left in
	// progress (ConflictedFiles populated) for an agent or the user to
	// resolve before pushing.
	ResolveMerge ResolveStrategy = "merge"
	// ResolveAbort cancels an in-progress merge, restoring the pre-merge
	// state.
	ResolveAbort ResolveStrategy = "abort"
)

// ResolveResult is the wire shape for POST /worktrees/{id}/remote/resolve.
type ResolveResult struct {
	Branch          string          `json:"branch"`
	Strategy        ResolveStrategy `json:"strategy"`
	State           RemoteState     `json:"state"`
	HeadSHA         string          `json:"head_sha,omitempty"`
	BackupRef       string          `json:"backup_ref,omitempty"`       // take_remote: where the discarded HEAD was saved
	ConflictedFiles []string        `json:"conflicted_files,omitempty"` // merge: unresolved paths (the client hands these to an agent)
}

// ResolveRemote reconciles a diverged worktree with its remote per
// strategy. The agent-merge flow is client-driven: pick ResolveMerge, and
// on a conflict result the client starts a session seeded to resolve the
// in-progress merge — so this method stays decoupled from session
// creation and never has to guess a backend.
func (s *Service) ResolveRemote(ctx context.Context, worktreeID string, strategy ResolveStrategy) (ResolveResult, error) {
	rc, err := s.remoteContextFor(ctx, worktreeID)
	if err != nil {
		return ResolveResult{}, err
	}
	res, err := runResolve(rc, strategy)
	if err == nil {
		s.log.Printf("resolve %s on %s (%s/%s) -> %s", strategy, rc.branch, rc.owner, rc.repo, res.State)
	}
	return res, err
}

// runResolve is the pure-git half. Testable against a local bare-repo
// remote.
func runResolve(rc remoteContext, strategy ResolveStrategy) (ResolveResult, error) {
	result := ResolveResult{Branch: rc.branch, Strategy: strategy}
	switch strategy {
	case ResolveAbort:
		if !git.IsMerging(rc.workdir) {
			return ResolveResult{}, ErrNotMerging
		}
		if err := git.AbortMerge(rc.workdir); err != nil {
			return ResolveResult{}, fmt.Errorf("abort merge: %w", err)
		}
		// Pre-merge local state; a status refresh refines it.
		result.State = RemoteStateUnpushed
		result.HeadSHA, _ = git.HeadCommit(rc.workdir)
		return result, nil

	case ResolveTakeRemote:
		if err := rc.fetchBranch(); err != nil {
			return ResolveResult{}, err
		}
		// HEAD here is the pre-merge local tip (a merge isn't committed
		// yet), which is exactly what we back up before discarding.
		head, err := git.HeadCommit(rc.workdir)
		if err != nil {
			return ResolveResult{}, fmt.Errorf("head commit: %w", err)
		}
		if git.IsMerging(rc.workdir) {
			if err := git.AbortMerge(rc.workdir); err != nil {
				return ResolveResult{}, fmt.Errorf("abort merge before reset: %w", err)
			}
		}
		backup := backupRefName(rc.branch, head)
		if err := git.BackupRef(rc.workdir, backup, head); err != nil {
			return ResolveResult{}, fmt.Errorf("backup ref: %w", err)
		}
		if err := git.ResetHard(rc.workdir, "FETCH_HEAD"); err != nil {
			return ResolveResult{}, fmt.Errorf("reset to remote: %w", err)
		}
		result.BackupRef = backup
		result.State = RemoteStateSynced
		result.HeadSHA, _ = git.HeadCommit(rc.workdir)
		return result, nil

	case ResolveMerge:
		if git.IsMerging(rc.workdir) {
			// Already mid-merge — report the outstanding conflicts rather
			// than starting a second merge.
			files, _ := git.ConflictedFiles(rc.workdir)
			result.ConflictedFiles = files
			result.State = RemoteStateConflict
			return result, nil
		}
		if err := rc.fetchBranch(); err != nil {
			return ResolveResult{}, err
		}
		// Commit uncommitted work (incl. untracked) first so the merge is
		// between commits.
		if err := git.AddAll(rc.workdir); err != nil {
			return ResolveResult{}, fmt.Errorf("git add -A: %w", err)
		}
		if staged, _ := git.HasStagedChanges(rc.workdir); staged {
			if err := git.Commit(rc.workdir, pushCommitMessage(rc.branch)); err != nil {
				return ResolveResult{}, fmt.Errorf("commit: %w", err)
			}
		}
		err := git.Merge(rc.workdir, "FETCH_HEAD", mergeCommitMessage(rc.branch))
		if err == nil {
			// The merge commit awaits a manual push.
			result.State = RemoteStateUnpushed
			result.HeadSHA, _ = git.HeadCommit(rc.workdir)
			return result, nil
		}
		if !errors.Is(err, git.ErrMergeConflict) {
			return ResolveResult{}, fmt.Errorf("merge: %w", err)
		}
		files, _ := git.ConflictedFiles(rc.workdir)
		result.ConflictedFiles = files
		result.State = RemoteStateConflict
		return result, nil

	default:
		return ResolveResult{}, fmt.Errorf("%w: unknown resolve strategy %q", ErrInvalidArgument, strategy)
	}
}

// backupRefName builds the fully-qualified ref a take_remote reset stashes
// the discarded HEAD under, so the work stays recoverable.
func backupRefName(branch, head string) string {
	short := head
	if len(short) > 8 {
		short = short[:8]
	}
	return "refs/clank/backup/" + sanitizeRefComponent(branch) + "-" + short
}

func sanitizeRefComponent(s string) string {
	return strings.NewReplacer("/", "-", " ", "-", "~", "-", "^", "-", ":", "-").Replace(s)
}
