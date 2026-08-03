package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/supaclank/clank/internal/git"
)

// PushResult is the wire shape for POST /worktrees/{id}/remote/push.
type PushResult struct {
	Branch    string `json:"branch"`
	HeadSHA   string `json:"head_sha"`
	Committed bool   `json:"committed"` // auto-committed uncommitted work before pushing
	Pushed    bool   `json:"pushed"`
}

// PushToRemote commits any uncommitted work in the worktree (hardcoded
// message) and pushes the branch to its GitHub remote. ErrRemoteDiverged
// when the remote has advanced and the push is rejected as non-fast-
// forward — the client then routes to the conflict-resolution flow.
// ErrNoCommonAncestor when the remote shares no history with the
// worktree (origin points at the wrong repo).
func (s *Service) PushToRemote(ctx context.Context, worktreeID string) (PushResult, error) {
	rc, err := s.remoteContextFor(ctx, worktreeID)
	if err != nil {
		return PushResult{}, err
	}
	res, err := runPush(rc)
	if err == nil {
		s.log.Printf("pushed %s to %s/%s (committed=%v)", rc.branch, rc.owner, rc.repo, res.Committed)
	}
	return res, err
}

// runPush is the pure-git half: verify the destination, commit-if-
// dirty, then push. Testable against a local bare-repo remote.
func runPush(rc remoteContext) (PushResult, error) {
	result := PushResult{Branch: rc.branch}

	// Wrong-repo safety net (mirrors CreatePR): refuse before
	// touching or shipping anything when origin's history is
	// unrelated to ours.
	if err := rc.verifyCommonHistory(); err != nil {
		return PushResult{}, err
	}

	committed, err := commitAllIfDirty(rc.workdir, rc.branch)
	if err != nil {
		return PushResult{}, err
	}
	result.Committed = committed

	headSHA, err := git.HeadCommit(rc.workdir)
	if err != nil {
		return PushResult{}, fmt.Errorf("head commit: %w", err)
	}
	result.HeadSHA = headSHA

	if err := git.Push(rc.workdir, rc.pushURL, rc.branch+":refs/heads/"+rc.branch, git.PushOptions{ExtraHeader: rc.authHeader}); err != nil {
		if errors.Is(err, git.ErrPushNotFastForward) {
			return PushResult{}, ErrRemoteDiverged
		}
		return PushResult{}, err
	}
	result.Pushed = true
	return result, nil
}
