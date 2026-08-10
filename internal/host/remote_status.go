package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/git"
	githubpkg "github.com/supaclank/clank/internal/host/github"
)

// RemoteStatusResult is the wire shape for GET /worktrees/{id}/remote/status.
// PRMergeable is present only once GitHub has computed the PR's test
// merge (absent = unknown, not clean) — a conflicting PR gets no CI
// runs, so clients surface the conflict where CI status would be.
type RemoteStatusResult struct {
	Branch       string                   `json:"branch"`
	Owner        string                   `json:"owner"`
	Repo         string                   `json:"repo"`
	State        RemoteState              `json:"state"`
	Ahead        int                      `json:"ahead"`
	Behind       int                      `json:"behind"`
	Dirty        bool                     `json:"dirty"`
	LocalHead    string                   `json:"local_head"`
	RemoteHead   string                   `json:"remote_head,omitempty"`
	PRNumber     int                      `json:"pr_number,omitempty"`
	PRURL        string                   `json:"pr_url,omitempty"`
	PRBaseBranch string                   `json:"pr_base_branch,omitempty"`
	PRDraft      bool                     `json:"pr_draft,omitempty"`
	PRMergeable  githubpkg.MergeableState `json:"pr_mergeable,omitempty"`
}

// RemoteSyncStatus reports where the ref's branch sits relative to
// its GitHub remote, plus the open PR (if any) for the branch. Does a
// network fetch — callers should refresh on demand, not poll tightly.
func (s *Service) RemoteSyncStatus(ctx context.Context, ref agent.GitRef) (RemoteStatusResult, error) {
	rc, err := s.remoteContextFor(ctx, ref)
	if err != nil {
		return RemoteStatusResult{}, err
	}
	result, err := computeStatus(rc)
	if err != nil {
		return RemoteStatusResult{}, err
	}
	s.attachPR(ctx, &result, rc)
	return result, nil
}

// computeStatus is the pure-git half of RemoteSyncStatus: fetch the
// branch, compare against the remote tip, classify. No GitHub API calls,
// so it's testable against a local bare-repo remote.
func computeStatus(rc remoteContext) (RemoteStatusResult, error) {
	localHead, err := git.HeadCommit(rc.workdir)
	if err != nil {
		return RemoteStatusResult{}, fmt.Errorf("head commit: %w", err)
	}
	dirty, err := git.WorkingTreeDirty(rc.workdir)
	if err != nil {
		return RemoteStatusResult{}, fmt.Errorf("check dirty: %w", err)
	}
	result := RemoteStatusResult{
		Branch:    rc.branch,
		Owner:     rc.owner,
		Repo:      rc.repo,
		LocalHead: localHead,
		Dirty:     dirty,
	}
	// A merge in progress is a conflict regardless of the counts.
	if git.IsMerging(rc.workdir) {
		result.State = RemoteStateConflict
		return result, nil
	}
	if err := rc.fetchBranch(); err != nil {
		if errors.Is(err, ErrNoUpstream) {
			result.State = RemoteStateNoUpstream
			return result, nil
		}
		return RemoteStatusResult{}, err
	}
	ahead, behind, err := git.AheadBehind(rc.workdir, "HEAD", "FETCH_HEAD")
	if err != nil {
		return RemoteStatusResult{}, fmt.Errorf("ahead/behind: %w", err)
	}
	result.Ahead = ahead
	result.Behind = behind
	result.RemoteHead, err = git.RevParse(rc.workdir, "FETCH_HEAD")
	if err != nil {
		return RemoteStatusResult{}, fmt.Errorf("rev-parse FETCH_HEAD: %w", err)
	}
	result.State = classifyRemoteState(ahead, behind, dirty)
	return result, nil
}

// attachPR best-effort fills PRNumber/PRURL/PRBaseBranch and the PR's
// mergeability from the open PR for the branch. Failures are logged,
// not fatal — the status is still useful without the PR annotations,
// and a failed mergeability fetch still leaves the deep-link intact.
func (s *Service) attachPR(ctx context.Context, result *RemoteStatusResult, rc remoteContext) {
	pr, err := s.github.FindOpenPRForBranch(ctx, rc.token, rc.owner, rc.repo, rc.branch)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Printf("remote status: find PR for %s/%s:%s: %v", rc.owner, rc.repo, rc.branch, err)
		}
		return
	}
	if pr == nil {
		return
	}
	result.PRNumber = pr.Number
	result.PRURL = pr.HTMLURL
	result.PRBaseBranch = pr.Base.Ref
	result.PRDraft = pr.Draft
	state, err := s.github.PRMergeable(ctx, rc.token, rc.owner, rc.repo, pr.Number)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Printf("remote status: PR mergeable for %s/%s#%d: %v", rc.owner, rc.repo, pr.Number, err)
		}
		return
	}
	if state != githubpkg.MergeableStateUnknown {
		result.PRMergeable = state
	}
}
