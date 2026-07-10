package host

// GET /repos/{slug}/overview — one repo's full work-item feed: every
// branch clank manages (refs/heads/*) UNION every open PR head, each
// annotated with worktree linkage, cheap local sync signals, and the
// PR. This is the single call that replaces the mobile repo screen's
// per-branch remote/status fan-out and powers the Drafts |
// Ready-for-review split. Per CLAUDE.md's per-method-file rule this
// operation lives on its own.
//
// Cost model: the git half is ZERO-network (for-each-ref tips, one
// worktree list, per-loaded-worktree dirty checks, ahead/behind against
// existing origin/* refs). ?fetch=1 adds exactly ONE authed all-heads
// fetch in the canonical, under the repo lock, so behind-counts are
// current. The PR half is two GitHub API list calls (open + recently
// closed) plus two bounded per-PR calls (check rollup + mergeability)
// per open PR head, all best-effort — an API failure degrades to what's
// built so far (or to PRs without that annotation) rather than failing
// the screen (mirroring attachPR in remote_status.go).

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// OverviewPRState is the lifecycle of an overview branch's PR. GitHub's
// list API reports merged PRs as "closed"; the overview splits them so
// clients can tell shipped work from abandoned work without a second call.
type OverviewPRState string

const (
	OverviewPRStateOpen   OverviewPRState = "open"
	OverviewPRStateMerged OverviewPRState = "merged"
	OverviewPRStateClosed OverviewPRState = "closed"
)

// OverviewPR is the PR annotation on an overview branch. IsMine compares
// the PR author against the connected GitHub login — the Drafts tab's
// "mine vs everyone" filter bit. State merged/closed marks a leftover
// local branch as finished work rather than an in-progress draft.
// Checks is the head commit's CI rollup, absent when the PR has no
// check runs (or the rollup fetch failed). Mergeable is present only
// once GitHub has computed the test merge (open PRs only) — conflicting
// PRs get no CI runs, so clients show the conflict where the CI badge
// would be.
type OverviewPR struct {
	Number    int                      `json:"number"`
	Title     string                   `json:"title"`
	State     OverviewPRState          `json:"state"`
	Draft     bool                     `json:"draft"`
	Author    string                   `json:"author"`
	URL       string                   `json:"url"`
	IsMine    bool                     `json:"is_mine"`
	UpdatedAt time.Time                `json:"updated_at,omitzero"`
	Checks    *githubpkg.CheckRollup   `json:"checks,omitempty"`
	Mergeable githubpkg.MergeableState `json:"mergeable,omitempty"`
}

// RepoBranchOverview is one work item: a branch (loaded or not) and/or
// its open PR. Ahead/Behind compare refs/heads/<branch> against
// refs/remotes/origin/<branch> and are present only when the tracking
// ref exists (omitted otherwise — absence means "no comparison
// available", not "in sync"). Dirty is computed only for loaded
// branches (it's a property of a worktree's working tree).
type RepoBranchOverview struct {
	Branch       string      `json:"branch"`
	WorktreeID   string      `json:"worktree_id,omitempty"`
	Loaded       bool        `json:"loaded"`
	Dirty        bool        `json:"dirty,omitempty"`
	Ahead        *int        `json:"ahead,omitempty"`
	Behind       *int        `json:"behind,omitempty"`
	LastCommitAt time.Time   `json:"last_commit_at,omitzero"`
	PR           *OverviewPR `json:"pr,omitempty"`
}

// RepoOverviewResult is the wire shape of GET /repos/{slug}/overview.
type RepoOverviewResult struct {
	Slug          string               `json:"slug"`
	Label         string               `json:"label"`
	Origin        *RepoOrigin          `json:"origin"`
	DefaultBranch string               `json:"default_branch"`
	Fetched       bool                 `json:"fetched"`
	Branches      []RepoBranchOverview `json:"branches"`
}

// RepoOverview assembles the feed for slug. fetch=true refreshes
// refs/remotes/origin/* first (one authed fetch; a no-op for
// unpublished repos). ErrRepoNotFound for an unknown slug.
func (s *Service) RepoOverview(ctx context.Context, slug string, fetch bool) (RepoOverviewResult, error) {
	gitDir, err := resolveRepoSlug(slug)
	if err != nil {
		return RepoOverviewResult{}, err
	}

	defer s.lockRepo(slug)()

	origin := repoOriginFor(gitDir)
	result := RepoOverviewResult{
		Slug:     slug,
		Label:    repoLabelFor(gitDir),
		Origin:   origin,
		Branches: []RepoBranchOverview{},
	}
	result.DefaultBranch, err = git.HeadBranch(gitDir)
	if err != nil {
		return RepoOverviewResult{}, err
	}

	// ?fetch=1 refreshes origin/* for ANY configured remote (github or
	// not — auth is resolved from the URL inside fetchAllHeads); a
	// greenfield repo with no remote at all is a quiet no-op.
	if fetch {
		remoteURL, rerr := git.RemoteURL(gitDir, "origin")
		switch {
		case rerr == nil && remoteURL != "":
			if err := s.fetchAllHeads(gitDir, remoteURL); err != nil {
				return RepoOverviewResult{}, err
			}
			result.Fetched = true
		case rerr != nil && !git.IsRemoteNotConfigured(rerr):
			return RepoOverviewResult{}, fmt.Errorf("get origin url: %w", rerr)
		}
	}

	// Git half: every clank-managed branch, annotated locally.
	tips, err := git.LocalBranchTips(gitDir)
	if err != nil {
		return RepoOverviewResult{}, err
	}
	loadedByBranch, err := loadedWorktreesByBranch(gitDir)
	if err != nil {
		return RepoOverviewResult{}, err
	}
	for _, tip := range tips {
		entry := RepoBranchOverview{Branch: tip.Branch, LastCommitAt: tip.CommittedAt}
		if wt, ok := loadedByBranch[tip.Branch]; ok {
			entry.Loaded = true
			entry.WorktreeID = wt.worktreeID
			if dirty, derr := git.WorkingTreeDirty(wt.path); derr == nil {
				entry.Dirty = dirty
			}
		}
		if tracking, terr := git.RemoteTrackingBranchExists(gitDir, "origin", tip.Branch); terr == nil && tracking {
			if ahead, behind, aerr := git.AheadBehind(gitDir, "refs/heads/"+tip.Branch, "refs/remotes/origin/"+tip.Branch); aerr == nil {
				entry.Ahead, entry.Behind = &ahead, &behind
			}
		}
		result.Branches = append(result.Branches, entry)
	}

	// PR half: best-effort merge of the repo's PRs, keyed by head
	// branch. Open PR-only heads (a colleague's branch you never
	// loaded) become loaded:false entries — the "check out" candidates;
	// merged/closed PRs mark leftover local branches as finished.
	s.attachRepoPRs(ctx, &result)

	// attachRepoPRs appends PR-only entries via map iteration (random
	// order in Go) — resort to restore the most-recently-active-first
	// contract LocalBranchTips established.
	sort.Slice(result.Branches, func(i, j int) bool {
		a, b := result.Branches[i], result.Branches[j]
		if !a.LastCommitAt.Equal(b.LastCommitAt) {
			return a.LastCommitAt.After(b.LastCommitAt)
		}
		return a.Branch < b.Branch
	})
	return result, nil
}

// fetchAllHeads refreshes refs/remotes/origin/* with one authed fetch.
// Caller holds the repo lock and passes the resolved origin URL.
func (s *Service) fetchAllHeads(gitDir, remoteURL string) error {
	fetchURL := remoteURL
	authHeader := ""
	if owner, repo, perr := githubpkg.ParseGitHubRemote(remoteURL); perr == nil {
		if s.github == nil {
			return ErrGitHubManagerUnavailable
		}
		creds, cerr := s.github.Store().Read()
		if cerr != nil {
			return fmt.Errorf("read github credentials: %w", cerr)
		}
		if creds.AccessToken == "" {
			return ErrGitHubNotConnected
		}
		fetchURL = fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
		authHeader = buildAuthHeader(creds.AccessToken)
	}
	if err := git.Fetch(gitDir, fetchURL, allHeadsFetchRefspec, git.PushOptions{ExtraHeader: authHeader}); err != nil {
		return fmt.Errorf("fetch origin heads: %w", err)
	}
	return nil
}

// allHeadsFetchRefspec mirrors every remote branch into origin/* — the
// same refspec CloneBare configures as the canonical's default.
const allHeadsFetchRefspec = "+refs/heads/*:refs/remotes/origin/*"

// loadedWorktree pairs a linked worktree's path with its stamped id.
type loadedWorktree struct {
	path       string
	worktreeID string
}

// loadedWorktreesByBranch maps branch → linked worktree from one
// `git worktree list` pass, skipping the bare entry, vanished dirs, and
// worktrees with unreadable ids.
func loadedWorktreesByBranch(gitDir string) (map[string]loadedWorktree, error) {
	worktrees, err := git.ListWorktrees(gitDir)
	if err != nil {
		return nil, err
	}
	byBranch := make(map[string]loadedWorktree, len(worktrees))
	for _, wt := range worktrees {
		if wt.Bare || wt.Branch == "" {
			continue
		}
		if _, statErr := os.Stat(wt.Path); statErr != nil {
			continue
		}
		id, idErr := agent.ReadLocalWorktreeID(wt.Path)
		if idErr != nil || id == "" {
			continue
		}
		byBranch[wt.Branch] = loadedWorktree{path: wt.Path, worktreeID: id}
	}
	return byBranch, nil
}

// attachRepoPRs merges the repo's PRs into the overview: annotate
// loaded/local branches whose head matches an open PR, append
// loaded:false entries for open PR heads with no local branch, and mark
// leftover local branches whose PR already merged/closed (so clients
// can file them under "Closed" instead of mistaking them for drafts).
// Best-effort — a GitHub API failure logs and returns what's built so
// far intact.
func (s *Service) attachRepoPRs(ctx context.Context, result *RepoOverviewResult) {
	if result.Origin == nil || s.github == nil {
		return
	}
	creds, err := s.github.Store().Read()
	if err != nil || creds.AccessToken == "" {
		return
	}
	open, err := s.github.ListPullRequests(ctx, creds.AccessToken, result.Origin.Owner, result.Origin.Repo, githubpkg.PRListStateOpen)
	if err != nil {
		s.log.Printf("repo overview: list open PRs for %s/%s: %v", result.Origin.Owner, result.Origin.Repo, err)
		return
	}
	byHead := make(map[string]*OverviewPR, len(open))
	for _, pr := range open {
		byHead[pr.HeadBranch] = wireOverviewPR(pr, creds.GitHubLogin)
	}
	// Checks are fetched for open PRs only — a merged/closed PR's CI
	// verdict is history, not a work signal worth the extra calls.
	s.attachPRChecks(ctx, creds.AccessToken, result.Origin, open, byHead)
	for i := range result.Branches {
		if pr, ok := byHead[result.Branches[i].Branch]; ok {
			result.Branches[i].PR = pr
			delete(byHead, result.Branches[i].Branch)
		}
	}
	for head, pr := range byHead {
		result.Branches = append(result.Branches, RepoBranchOverview{
			Branch: head,
			Loaded: false,
			// TODO(ai-review): PR-only entries stand in PR UpdatedAt (review/comment
			// activity) for LastCommitAt (tip commit time) — sort/consumer contract
			// says "commit time" but this can be neither. https://github.com/Acksell/clank/pull/95#discussion_r3512692389
			LastCommitAt: pr.UpdatedAt,
			PR:           pr,
		})
	}
	s.attachClosedRepoPRs(ctx, result, creds)
}

// attachClosedRepoPRs marks local branches whose PR merged or closed.
// Only annotates branches with no open PR (branch reuse: the open PR
// wins), and never appends PR-only entries — a colleague's merged PR
// with no local branch isn't a work item. The listing is sorted most
// recently updated first, so the first closed PR per head wins.
//
// TODO: the listing is capped at maxPulls (100) most recently updated
// closed PRs, so a branch merged long ago in a very active repo can
// still come back unannotated and land in Drafts. If that bites, query
// per-head (GET /pulls?head=owner:branch) for the leftover branches
// instead of one bulk list.
func (s *Service) attachClosedRepoPRs(ctx context.Context, result *RepoOverviewResult, creds githubpkg.Credentials) {
	closed, err := s.github.ListPullRequests(ctx, creds.AccessToken, result.Origin.Owner, result.Origin.Repo, githubpkg.PRListStateClosed)
	if err != nil {
		s.log.Printf("repo overview: list closed PRs for %s/%s: %v", result.Origin.Owner, result.Origin.Repo, err)
		return
	}
	byHead := make(map[string]githubpkg.PullRequestSummary, len(closed))
	for _, pr := range closed {
		if _, ok := byHead[pr.HeadBranch]; !ok {
			byHead[pr.HeadBranch] = pr
		}
	}
	for i := range result.Branches {
		if result.Branches[i].PR != nil {
			continue
		}
		if pr, ok := byHead[result.Branches[i].Branch]; ok {
			result.Branches[i].PR = wireOverviewPR(pr, creds.GitHubLogin)
		}
	}
}

// wireOverviewPR collapses a PR summary to the overview annotation,
// splitting GitHub's "closed" into merged vs closed via MergedAt.
func wireOverviewPR(pr githubpkg.PullRequestSummary, login string) *OverviewPR {
	state := OverviewPRState(pr.State)
	if state == OverviewPRStateClosed && !pr.MergedAt.IsZero() {
		state = OverviewPRStateMerged
	}
	return &OverviewPR{
		Number:    pr.Number,
		Title:     pr.Title,
		State:     state,
		Draft:     pr.Draft,
		Author:    pr.Author,
		URL:       pr.HTMLURL,
		IsMine:    login != "" && pr.Author == login,
		UpdatedAt: pr.UpdatedAt,
	}
}

// checkRollupConcurrency bounds the per-PR annotation fan-out so a repo
// with many open PRs doesn't burst the GitHub API.
const checkRollupConcurrency = 8

// attachPRChecks annotates each overview PR with its head commit's CI
// rollup and its mergeability, two bounded API calls per PR. Best-effort
// per PR and per call — a failed fetch leaves just that annotation off;
// failures are logged once, aggregated.
func (s *Service) attachPRChecks(ctx context.Context, token string, origin *RepoOrigin, pulls []githubpkg.PullRequestSummary, byHead map[string]*OverviewPR) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, checkRollupConcurrency)
	var mu sync.Mutex
	failed := 0
	var firstErr error
	record := func(err error) {
		mu.Lock()
		failed++
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	for _, pull := range pulls {
		pr := byHead[pull.HeadBranch]
		// byHead keeps one entry per head branch name; a PR that lost that
		// slot to another PR sharing the same head branch must not fetch
		// into the survivor's annotations (data race + wrong association).
		if pr == nil || pr.Number != pull.Number {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if pull.HeadSHA != "" {
				rollup, err := s.github.CheckRollupForRef(ctx, token, origin.Owner, origin.Repo, pull.HeadSHA)
				if err != nil {
					record(err)
				} else {
					pr.Checks = rollup
				}
			}
			state, err := s.github.PRMergeable(ctx, token, origin.Owner, origin.Repo, pull.Number)
			if err != nil {
				record(err)
			} else if state != githubpkg.MergeableStateUnknown {
				pr.Mergeable = state
			}
		}()
	}
	wg.Wait()
	if failed > 0 {
		s.log.Printf("repo overview: PR annotations for %s/%s: %d fetches failed, first: %v",
			origin.Owner, origin.Repo, failed, firstErr)
	}
}
