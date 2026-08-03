package host

// Worktree↔GitHub-remote sync. Shared resolution + state classification
// for the remote_status / remote_push / remote_pull / remote_resolve
// endpoints. "Remote" here is the git origin on GitHub — the only sync
// axis clank has.
//
// Per CLAUDE.md's per-method-file rule, each operation lives in its own
// remote_*.go; this file holds only what they share.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/git"
	githubpkg "github.com/supaclank/clank/internal/host/github"
)

// RemoteState classifies a worktree's branch relative to origin/<branch>.
type RemoteState string

const (
	RemoteStateSynced     RemoteState = "synced"      // local == remote, clean tree
	RemoteStateUnpushed   RemoteState = "unpushed"    // local ahead and/or dirty, remote not ahead
	RemoteStateBehind     RemoteState = "behind"      // remote ahead, fast-forwardable
	RemoteStateDiverged   RemoteState = "diverged"    // both sides advanced — needs resolution
	RemoteStateConflict   RemoteState = "conflict"    // a merge is in progress with conflicts
	RemoteStateNoUpstream RemoteState = "no_upstream" // branch not on the remote yet
)

// Remote-sync errors. Each maps to a distinct HTTP status + machine code
// in the mux handler. Reuses ErrGitHubManagerUnavailable /
// ErrGitHubNotConnected / ErrNoOriginRemote from github_pr.go.
var (
	// ErrRemoteDiverged fires when local and origin/<branch> have each
	// advanced past their merge-base: a push is rejected (non-fast-
	// forward) and a pull can't fast-forward. Routes to conflict
	// resolution. 409.
	ErrRemoteDiverged = errors.New("host: local and remote have diverged")

	// ErrWorktreeDirty fires when a pull is requested but the worktree has
	// uncommitted changes a fast-forward could clobber. 409.
	ErrWorktreeDirty = errors.New("host: worktree has uncommitted changes; commit or push them first")

	// ErrNoUpstream fires when the branch doesn't exist on the remote yet
	// — nothing to pull or resolve against. 400.
	ErrNoUpstream = errors.New("host: branch has no upstream on the remote")

	// ErrDetachedHead fires when the worktree is on a detached HEAD, so
	// there's no branch to push or pull. 409.
	ErrDetachedHead = errors.New("host: worktree is on a detached HEAD")

	// ErrNotMerging fires when an abort is requested but no merge is in
	// progress. 409.
	ErrNotMerging = errors.New("host: no merge in progress")
)

// remoteContext bundles everything a remote-sync operation needs: the
// resolved working dir, the worktree's branch, and the GitHub destination
// + auth. Built once per request by remoteContextFor. The run* helpers
// take it (not a worktree ID) so they're testable against a local
// bare-repo remote without GitHub.
type remoteContext struct {
	workdir    string
	branch     string
	owner      string
	repo       string
	pushURL    string // https://github.com/<owner>/<repo>.git (a local path in tests)
	authHeader string // git http.extraheader value; empty for unauthenticated remotes
	token      string // GitHub access token, for PR lookups
}

// remoteContextFor resolves a worktree to its working dir and GitHub
// remote, reading the connected token. Mirrors CreatePR's resolution. The
// github.com-origin requirement lives here so the run* helpers stay
// transport-agnostic.
func (s *Service) remoteContextFor(_ context.Context, worktreeID string) (remoteContext, error) {
	if s.github == nil {
		return remoteContext{}, ErrGitHubManagerUnavailable
	}
	workdir, err := s.repoRootFor(agent.GitRef{WorktreeID: worktreeID})
	if err != nil {
		return remoteContext{}, fmt.Errorf("resolve worktree: %w", err)
	}
	token, err := s.github.AccessToken()
	if err != nil {
		if errors.Is(err, githubpkg.ErrNotConnected) {
			return remoteContext{}, ErrGitHubNotConnected
		}
		return remoteContext{}, fmt.Errorf("github token: %w", err)
	}
	branch, err := git.CurrentBranch(workdir)
	if err != nil {
		return remoteContext{}, fmt.Errorf("current branch: %w", err)
	}
	if branch == "HEAD" {
		return remoteContext{}, ErrDetachedHead
	}
	remoteURL, err := git.RemoteURL(workdir, "origin")
	if err != nil {
		return remoteContext{}, ErrNoOriginRemote
	}
	owner, repo, err := githubpkg.ParseGitHubRemote(remoteURL)
	if err != nil {
		return remoteContext{}, fmt.Errorf("parse origin url %q: %w", remoteURL, err)
	}
	return remoteContext{
		workdir:    workdir,
		branch:     branch,
		owner:      owner,
		repo:       repo,
		pushURL:    fmt.Sprintf("https://github.com/%s/%s.git", owner, repo),
		authHeader: buildAuthHeader(token),
		token:      token,
	}, nil
}

// fetchBranch refreshes origin/<branch> into FETCH_HEAD using the
// context's auth. Returns ErrNoUpstream when the branch doesn't exist on
// the remote (vs a transport/auth failure, which surfaces wrapped).
// TODO(ai-review): no context.Context — git.Fetch can hang on bad network https://github.com/supaclank/clank/pull/84#discussion_r3486368618
func (rc remoteContext) fetchBranch() error {
	err := git.Fetch(rc.workdir, rc.pushURL, rc.branch, git.PushOptions{ExtraHeader: rc.authHeader})
	if err == nil {
		return nil
	}
	if isNoRemoteRef(err) {
		return ErrNoUpstream
	}
	return fmt.Errorf("fetch remote branch %q: %w", rc.branch, err)
}

// verifyCommonHistory refuses a push destination whose history is
// unrelated to the worktree's — the same wrong-repo safety net
// CreatePR has. Fetches the remote's HEAD (its default branch; the
// worktree's own branch may not exist there yet) and requires a
// merge-base with local HEAD: unrelated repos share no commits, so an
// empty merge-base almost certainly means origin points at the wrong
// repo. A remote with no HEAD at all (fresh empty repo) passes —
// there is no history to diverge from, and a manual first push to a
// just-created repo is legitimate.
func (rc remoteContext) verifyCommonHistory() error {
	if err := git.Fetch(rc.workdir, rc.pushURL, "HEAD", git.PushOptions{ExtraHeader: rc.authHeader}); err != nil {
		if isNoRemoteRef(err) {
			return nil
		}
		return fmt.Errorf("%w: %v", ErrBaseRefUnreachable, err)
	}
	mb, err := git.MergeBase(rc.workdir, "FETCH_HEAD", "HEAD")
	if err != nil {
		return fmt.Errorf("verify common history: %w", err)
	}
	if mb == "" {
		return ErrNoCommonAncestor
	}
	return nil
}

// classifyRemoteState maps ahead/behind counts + dirtiness to a
// RemoteState. A merge-in-progress (RemoteStateConflict) is detected
// separately by the caller via git.IsMerging.
func classifyRemoteState(ahead, behind int, dirty bool) RemoteState {
	switch {
	case behind > 0 && (ahead > 0 || dirty):
		return RemoteStateDiverged
	case behind > 0:
		return RemoteStateBehind
	case ahead > 0 || dirty:
		return RemoteStateUnpushed
	default:
		return RemoteStateSynced
	}
}

// isNoRemoteRef reports whether a fetch error means the branch isn't on
// the remote, as opposed to a transport/auth failure.
func isNoRemoteRef(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "couldn't find remote ref") ||
		strings.Contains(msg, "no such ref") ||
		strings.Contains(msg, "not our ref")
}
