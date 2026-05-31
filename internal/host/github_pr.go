package host

// Orchestration for "open a PR from a worktree". Combines:
//   - Worktree resolution (via Service.workDirFor)
//   - Credential read (via the github.Manager's Store)
//   - Branch + remote inspection (via internal/git)
//   - The actual push + API call (via internal/git.Push and
//     github.Manager.CreatePullRequest)
//
// Per-method file matches CLAUDE.md's "per-method files for a struct,
// especially endpoints" rule. The mux handler lives in
// internal/host/mux/github_pr.go.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// CreatePRRequest is the wire shape for POST /worktrees/{id}/pr.
// All fields except Draft are required — no fallbacks (per
// CLAUDE.md). The head branch is derived from the worktree's
// current branch (the worktree IS its branch by construction).
type CreatePRRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Base  string `json:"base"`
	Draft bool   `json:"draft"`
}

// CreatePRResult is the wire shape for the 201 response. Includes
// head_branch and base_branch so the client doesn't have to remember
// what it sent.
type CreatePRResult struct {
	PRNumber   int    `json:"pr_number"`
	PRURL      string `json:"pr_url"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch"`
	HeadSHA    string `json:"head_sha"`
}

// PreviewOriginState classifies what we know about the worktree's
// origin before any network calls. Drives the mobile-side
// "Open PR to <owner>/<repo>" callout vs. the no-origin/non-github
// CTAs.
type PreviewOriginState string

const (
	// PreviewOriginGitHub: origin is set and parses to a github.com URL.
	// Safe to enable the Open PR form.
	PreviewOriginGitHub PreviewOriginState = "github"
	// PreviewOriginNone: no `origin` remote on the worktree.
	PreviewOriginNone PreviewOriginState = "no_origin"
	// PreviewOriginNonGitHub: origin is set but points elsewhere
	// (GitLab, Gitea, GHE, ...). NonGitHubHost carries the host for
	// the error message.
	PreviewOriginNonGitHub PreviewOriginState = "non_github"
)

// PreviewPRResult is the wire shape for the preview endpoint. The
// mobile CreatePRSheet uses Owner/Repo to render the destination
// callout, and OriginState to gate which form variant to show.
type PreviewPRResult struct {
	Owner         string             `json:"owner,omitempty"`
	Repo          string             `json:"repo,omitempty"`
	HeadBranch    string             `json:"head_branch,omitempty"`
	HeadSHA       string             `json:"head_sha,omitempty"`
	OriginState   PreviewOriginState `json:"origin_state"`
	NonGitHubHost string             `json:"non_github_host,omitempty"`
}

// Errors specific to CreatePR. Each one maps to a distinct HTTP
// status + machine-readable code in the mux handler.
var (
	// ErrGitHubManagerUnavailable fires when the host couldn't
	// instantiate the github.Manager at startup (homedir resolution
	// failure). 503.
	ErrGitHubManagerUnavailable = errors.New("github manager unavailable")

	// ErrGitHubNotConnected fires when the credential file is
	// absent. 403; client UI surfaces a "Connect GitHub" CTA.
	ErrGitHubNotConnected = errors.New("github not connected")

	// ErrPRMissingField fires when the request body omits a required
	// field. 400.
	ErrPRMissingField = errors.New("pr request missing required field")

	// ErrNothingToPush fires when the worktree's branch is at or
	// behind the base — no commits to PR. 400; client UI shows
	// "nothing to push yet" hint.
	ErrNothingToPush = errors.New("nothing to push: branch is up to date with base")

	// ErrNoOriginRemote fires when the worktree has no `origin`
	// remote configured. Common on worktrees materialized onto a sprite whose
	// `.git/config` didn't carry over with the bundle. 400; client
	// UI suggests "git remote add origin <github-url>" or
	// re-pushing from laptop with the remote intact.
	ErrNoOriginRemote = errors.New("worktree has no 'origin' remote — clank sync may have stripped .git/config; add it manually or re-push from laptop")

	// ErrNoCommonAncestor fires when the head branch and the
	// remote's base have no shared history. Overwhelmingly indicates
	// the remote points at the wrong repo (two unrelated repos
	// virtually never share a commit SHA). Hard refusal — no bypass.
	// 409.
	ErrNoCommonAncestor = errors.New("no common ancestor with remote base — origin probably points to the wrong repo")

	// ErrBaseRefUnreachable fires when we couldn't fetch the base
	// branch from the remote. Without a fresh origin/<base>, the
	// common-ancestor check can't run safely, so we refuse rather
	// than push blindly. 502.
	ErrBaseRefUnreachable = errors.New("could not fetch base branch from remote — cannot safely verify common history")
)

// CreatePR pushes the worktree's current branch and opens a pull
// request against base. Errors surface verbatim — the mux handler
// classifies them into HTTP statuses; the github package's typed
// errors (ErrPRAlreadyExists, ErrPushNotFastForward, etc.) pass
// through.
func (s *Service) CreatePR(ctx context.Context, worktreeID string, req CreatePRRequest) (CreatePRResult, error) {
	if req.Title == "" {
		return CreatePRResult{}, fmt.Errorf("%w: title", ErrPRMissingField)
	}
	if req.Base == "" {
		return CreatePRResult{}, fmt.Errorf("%w: base", ErrPRMissingField)
	}
	if s.github == nil {
		return CreatePRResult{}, ErrGitHubManagerUnavailable
	}

	// Resolve the worktree to a working directory. Reuses the same
	// resolver every other Service method uses — keeps the WorktreeID →
	// path mapping in one place.
	workdir, err := s.workDirFor(ctx, agent.GitRef{WorktreeID: worktreeID})
	if err != nil {
		return CreatePRResult{}, fmt.Errorf("resolve worktree: %w", err)
	}

	creds, err := s.github.Store().Read()
	if err != nil {
		return CreatePRResult{}, fmt.Errorf("read github credentials: %w", err)
	}
	if creds.AccessToken == "" {
		return CreatePRResult{}, ErrGitHubNotConnected
	}

	branch, err := git.CurrentBranch(workdir)
	if err != nil {
		return CreatePRResult{}, fmt.Errorf("current branch: %w", err)
	}
	if branch == "HEAD" || branch == req.Base {
		return CreatePRResult{}, fmt.Errorf("%w: worktree is on %q", ErrNothingToPush, branch)
	}

	headSHA, err := git.HeadCommit(workdir)
	if err != nil {
		return CreatePRResult{}, fmt.Errorf("head commit: %w", err)
	}

	remoteURL, err := git.RemoteURL(workdir, "origin")
	if err != nil {
		// git config errors out when the key isn't set — treat any
		// failure here as "no origin." Avoid leaking the raw stderr
		// (which contains the literal config key) into the response.
		return CreatePRResult{}, ErrNoOriginRemote
	}
	owner, repo, err := githubpkg.ParseGitHubRemote(remoteURL)
	if err != nil {
		return CreatePRResult{}, fmt.Errorf("parse origin url %q: %w", remoteURL, err)
	}

	authHeader := buildAuthHeader(creds.AccessToken)

	// Push and fetch use an explicit HTTPS URL built from the parsed
	// (owner, repo), not the literal "origin". This decouples the
	// PR-time transport from whatever URL form was configured on the
	// worktree (SSH, HTTPS with embedded creds, missing): we always
	// speak HTTPS to github.com with the OAuth token injected via
	// http.extraheader.
	pushURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)

	// Fetch origin/<base> to ensure we have an up-to-date reference
	// before the common-ancestor check. Promoted from best-effort to
	// hard requirement: a failed fetch means we can't safely verify
	// history overlap, so we refuse rather than push blindly.
	if err := git.Fetch(workdir, pushURL, req.Base, git.PushOptions{
		ExtraHeader: authHeader,
	}); err != nil {
		s.log.Printf("CreatePR: fetch base %s from %s: %v", req.Base, pushURL, err)
		return CreatePRResult{}, ErrBaseRefUnreachable
	}

	// Safety net: refuse if our branch shares no history with the
	// remote's base. This catches "origin points at the wrong repo"
	// regardless of how origin got mis-set — manual edit, agent
	// confusion, stale .git/config from a materialized worktree, etc.
	// Two repos with unrelated histories share no commits (Git
	// content-addresses everything), so an empty merge-base is a
	// near-certain wrong-destination signal.
	//
	// git.Fetch above writes to FETCH_HEAD (and to refs/remotes/origin/<base>
	// if origin happens to be configured to map there). We check
	// against FETCH_HEAD specifically — it's always populated by
	// the most recent fetch, regardless of how origin is configured.
	mb, err := git.MergeBase(workdir, "FETCH_HEAD", "HEAD")
	if err != nil {
		s.log.Printf("CreatePR: merge-base FETCH_HEAD HEAD: %v", err)
		return CreatePRResult{}, fmt.Errorf("verify common history: %w", err)
	}
	if mb == "" {
		return CreatePRResult{}, ErrNoCommonAncestor
	}

	// Verify there's actually something to push. After the fetch
	// above, FETCH_HEAD is the freshly-fetched remote base. If our
	// branch is at or behind it, there's nothing new to PR.
	ahead, err := commitsAhead(workdir, "FETCH_HEAD", branch)
	if err != nil {
		// commits-ahead failing here is unexpected — we just
		// successfully merge-base'd against FETCH_HEAD. Log + trust
		// the GitHub API to reject empty diffs.
		s.log.Printf("CreatePR: commits-ahead FETCH_HEAD %s: %v", branch, err)
		ahead = 1
	}
	if ahead == 0 {
		return CreatePRResult{}, ErrNothingToPush
	}

	// Push the branch with the OAuth token injected via process-local
	// git config. The header lives in args for the subprocess only —
	// never written to .git/config or the remote URL.
	if err := git.Push(workdir, pushURL, branch+":refs/heads/"+branch, git.PushOptions{
		ExtraHeader: authHeader,
	}); err != nil {
		return CreatePRResult{}, err
	}

	pr, err := s.github.CreatePullRequest(ctx, creds.AccessToken, owner, repo, githubpkg.CreatePRInput{
		Title: req.Title,
		Body:  req.Body,
		Head:  branch,
		Base:  req.Base,
		Draft: req.Draft,
	})
	if err != nil {
		return CreatePRResult{}, err
	}

	return CreatePRResult{
		PRNumber:   pr.Number,
		PRURL:      pr.HTMLURL,
		HeadBranch: branch,
		BaseBranch: req.Base,
		HeadSHA:    headSHA,
	}, nil
}

// PreviewPR returns what a CreatePR call WOULD push without actually
// pushing. The mobile CreatePRSheet calls this to show the user the
// parsed destination repo before they tap Open PR — primary UX
// defense against wrong-repo-because-origin-is-misconfigured.
//
// Cheap: no fetch, no network calls, no GitHub API requests. Just
// resolves the worktree, reads HEAD, classifies origin.
func (s *Service) PreviewPR(ctx context.Context, worktreeID string) (PreviewPRResult, error) {
	workdir, err := s.workDirFor(ctx, agent.GitRef{WorktreeID: worktreeID})
	if err != nil {
		return PreviewPRResult{}, fmt.Errorf("resolve worktree: %w", err)
	}

	branch, err := git.CurrentBranch(workdir)
	if err != nil {
		return PreviewPRResult{}, fmt.Errorf("current branch: %w", err)
	}
	headSHA, err := git.HeadCommit(workdir)
	if err != nil {
		return PreviewPRResult{}, fmt.Errorf("head commit: %w", err)
	}

	result := PreviewPRResult{
		HeadBranch: branch,
		HeadSHA:    headSHA,
	}

	remoteURL, err := git.RemoteURL(workdir, "origin")
	if err != nil {
		result.OriginState = PreviewOriginNone
		return result, nil
	}
	owner, repo, err := githubpkg.ParseGitHubRemote(remoteURL)
	if err != nil {
		// Non-github origin (gitlab, gitea, GHE, ...). Surface the
		// host so the UI can render a clear "only GitHub is
		// supported" message naming the actual destination.
		result.OriginState = PreviewOriginNonGitHub
		result.NonGitHubHost = githubpkg.RemoteHost(remoteURL)
		return result, nil
	}
	result.OriginState = PreviewOriginGitHub
	result.Owner = owner
	result.Repo = repo
	return result, nil
}

// buildAuthHeader assembles the http.extraheader value git uses to
// authenticate. Format: "Authorization: Basic <b64(x-access-token:tok)>".
// `x-access-token` is GitHub's documented username for OAuth-app-issued
// tokens used over HTTPS Basic auth — same pattern `gh` itself uses.
func buildAuthHeader(token string) string {
	enc := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return "Authorization: Basic " + enc
}

// commitsAhead is a tiny adapter so the orchestrator can swap base
// reference forms without restructuring. Returns an error iff the
// underlying git call errors; otherwise the count.
func commitsAhead(workdir, base, branch string) (int, error) {
	return git.CommitsAhead(workdir, base, branch)
}
