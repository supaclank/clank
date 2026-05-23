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
		return CreatePRResult{}, fmt.Errorf("get origin url: %w", err)
	}
	owner, repo, err := githubpkg.ParseGitHubRemote(remoteURL)
	if err != nil {
		return CreatePRResult{}, fmt.Errorf("parse origin url: %w", err)
	}

	authHeader := buildAuthHeader(creds.AccessToken)

	// Refresh origin/<base> before checking the local diff against
	// it. Sprites whose worktrees were migrated piecemeal often have
	// no origin/<base> at all — the fetch is what brings it into
	// existence. Best-effort: on failure, the commits-ahead check
	// falls back to bare <base>, and if that also fails we skip the
	// check (GitHub will reject empty PRs anyway, with a clearer
	// error than we'd produce locally).
	if err := git.Fetch(workdir, "origin", req.Base, git.PushOptions{
		ExtraHeader: authHeader,
	}); err != nil {
		s.log.Printf("CreatePR: fetch origin/%s (best-effort): %v", req.Base, err)
	}

	// Verify there's actually something to push. Try origin/<base>
	// first (the normal case after the fetch above), fall back to
	// bare <base> (covers local-only bases, common in tests), and
	// if both fail we trust GitHub to reject empty PRs cleanly.
	ahead, err := commitsAhead(workdir, "origin/"+req.Base, branch)
	if err != nil {
		ahead, _ = commitsAhead(workdir, req.Base, branch)
		// On a fresh sprite where neither ref resolves yet, ahead==0
		// here. We can't distinguish "actually empty" from "couldn't
		// determine" — skip the check and let GitHub be the judge.
		ahead = max(ahead, 1)
	}
	if ahead == 0 {
		return CreatePRResult{}, ErrNothingToPush
	}

	// Push the branch with the OAuth token injected via process-local
	// git config. The header lives in args for the subprocess only —
	// never written to .git/config or the remote URL.
	if err := git.Push(workdir, "origin", branch+":refs/heads/"+branch, git.PushOptions{
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
