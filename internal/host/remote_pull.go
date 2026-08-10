package host

import (
	"context"
	"fmt"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/git"
)

// PullResult is the wire shape for POST /worktrees/{id}/remote/pull.
type PullResult struct {
	Branch        string      `json:"branch"`
	State         RemoteState `json:"state"`
	HeadSHA       string      `json:"head_sha"`
	FastForwarded bool        `json:"fast_forwarded"`
}

// PullFromRemote fast-forwards the worktree's branch to its GitHub remote
// when it's cleanly behind. Refuses on a dirty tree (ErrWorktreeDirty) or
// when the histories diverged (ErrRemoteDiverged) — divergence routes to
// the conflict-resolution flow. Also invoked by cold-start auto-pull.
func (s *Service) PullFromRemote(ctx context.Context, ref agent.GitRef) (PullResult, error) {
	rc, err := s.remoteContextFor(ctx, ref)
	if err != nil {
		return PullResult{}, err
	}
	res, err := runPull(rc)
	if err == nil && res.FastForwarded {
		s.log.Printf("fast-forwarded %s from %s/%s", rc.branch, rc.owner, rc.repo)
	}
	return res, err
}

// runPull is the pure-git half: refuse-if-dirty, fetch, fast-forward when
// cleanly behind. Testable against a local bare-repo remote.
func runPull(rc remoteContext) (PullResult, error) {
	// Use IsClean (not WorkingTreeDirty) — fast-forward preserves untracked
	// files, so only tracked modifications are a real blocker.
	clean, err := git.IsClean(rc.workdir)
	if err != nil {
		return PullResult{}, fmt.Errorf("check clean: %w", err)
	}
	if !clean {
		return PullResult{}, ErrWorktreeDirty
	}
	if err := rc.fetchBranch(); err != nil {
		return PullResult{}, err
	}
	ahead, behind, err := git.AheadBehind(rc.workdir, "HEAD", "FETCH_HEAD")
	if err != nil {
		return PullResult{}, fmt.Errorf("ahead/behind: %w", err)
	}
	result := PullResult{Branch: rc.branch}
	switch {
	case behind == 0:
		// Nothing to pull; the branch may still be ahead (unpushed).
		result.State = classifyRemoteState(ahead, 0, false)
	case ahead > 0:
		return PullResult{}, ErrRemoteDiverged
	default:
		if err := git.MergeFF(rc.workdir, "FETCH_HEAD"); err != nil {
			return PullResult{}, fmt.Errorf("fast-forward: %w", err)
		}
		result.FastForwarded = true
		result.State = RemoteStateSynced
	}
	head, err := git.HeadCommit(rc.workdir)
	if err != nil {
		return PullResult{}, fmt.Errorf("head commit: %w", err)
	}
	result.HeadSHA = head
	return result, nil
}
