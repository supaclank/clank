package host

// POST /worktrees/{id}/remote/publish — create a fresh (private by default)
// GitHub repo for a greenfield, remote-less worktree, wire it as origin, and
// push. The create-side counterpart to remote_push.go's "push to an existing
// origin". Per CLAUDE.md's per-method-file rule this lives on its own.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/git"
	githubpkg "github.com/supaclank/clank/internal/host/github"
)

var (
	// ErrAlreadyPublished fires when publish is called on a worktree that
	// already has an origin remote — it's already on GitHub, so push (not
	// publish) is the right operation. 409.
	ErrAlreadyPublished = errors.New("host: worktree already has an origin remote")

	// ErrInvalidRepoName fires when the requested name is empty or sanitizes
	// to nothing usable. 400.
	ErrInvalidRepoName = errors.New("host: a valid repository name is required")
)

// PublishRequest is the body for POST /worktrees/{id}/remote/publish. Name is
// sanitized to GitHub's allowed characters; Private defaults to true when
// omitted.
type PublishRequest struct {
	Name    string `json:"name,omitempty"`
	Private *bool  `json:"private,omitempty"`
}

// PublishResult is the wire shape returned after a successful publish. The
// client refetches remote/status afterward to pick up the normal
// push/pull/PR UI (origin now exists).
type PublishResult struct {
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	HTMLURL string `json:"html_url"`
	HeadSHA string `json:"head_sha"`
}

// PublishToRemote creates a brand-new repository (private by default) for a
// remote-less worktree, adds it as origin, commits any uncommitted work, and
// pushes the current branch. ErrAlreadyPublished when origin already exists.
//
// TODO(ai-review): only sanitizeRepoName is covered by tests; the orchestration
// itself (create repo, add origin, commit, push, error mapping) has no
// integration coverage yet. https://github.com/supaclank/clank/pull/90#discussion_r3508931108
func (s *Service) PublishToRemote(ctx context.Context, ref agent.GitRef, req PublishRequest) (PublishResult, error) {
	if s.github == nil {
		return PublishResult{}, ErrGitHubManagerUnavailable
	}
	name := sanitizeRepoName(req.Name)
	if name == "" {
		return PublishResult{}, ErrInvalidRepoName
	}
	workdir, err := s.repoRootFor(ref)
	if err != nil {
		return PublishResult{}, fmt.Errorf("resolve worktree: %w", err)
	}
	token, err := s.github.AccessToken()
	if err != nil {
		if errors.Is(err, githubpkg.ErrNotConnected) {
			return PublishResult{}, ErrGitHubNotConnected
		}
		return PublishResult{}, fmt.Errorf("github token: %w", err)
	}
	// Already wired to a remote → publishing would clobber; the caller should
	// push instead. RemoteURL errors when there's no origin — that's our path.
	if _, err := git.RemoteURL(workdir, "origin"); err == nil {
		return PublishResult{}, ErrAlreadyPublished
	}
	branch, err := git.CurrentBranch(workdir)
	if err != nil {
		return PublishResult{}, fmt.Errorf("current branch: %w", err)
	}
	if branch == "HEAD" {
		return PublishResult{}, ErrDetachedHead
	}

	// Commit any uncommitted work so there's a HEAD to push (mirrors runPush's
	// stage-then-commit; scaffolds carry template commits but the session may
	// have dirtied the tree).
	if err := git.AddAll(workdir); err != nil {
		return PublishResult{}, fmt.Errorf("git add -A: %w", err)
	}
	staged, err := git.HasStagedChanges(workdir)
	if err != nil {
		return PublishResult{}, fmt.Errorf("check staged: %w", err)
	}
	if staged {
		if err := git.Commit(workdir, pushCommitMessage(branch)); err != nil {
			return PublishResult{}, fmt.Errorf("commit: %w", err)
		}
	}
	headSHA, err := git.HeadCommit(workdir)
	if err != nil {
		return PublishResult{}, fmt.Errorf("head commit: %w", err)
	}

	private := true
	if req.Private != nil {
		private = *req.Private
	}
	created, err := s.github.CreateRepository(ctx, token, githubpkg.CreateRepoInput{
		Name:    name,
		Private: private,
	})
	if err != nil {
		return PublishResult{}, err
	}

	pushURL := created.CloneURL
	if err := git.RemoteAdd(workdir, "origin", pushURL); err != nil {
		return PublishResult{}, fmt.Errorf("add origin: %w", err)
	}
	if err := git.Push(workdir, pushURL, branch+":refs/heads/"+branch, git.PushOptions{ExtraHeader: buildAuthHeader(token)}); err != nil {
		return PublishResult{}, err
	}
	s.log.Printf("published %s to %s/%s (branch %s)", workdir, created.Owner, created.Name, branch)
	return PublishResult{
		Owner:   created.Owner,
		Repo:    created.Name,
		Branch:  branch,
		HTMLURL: created.HTMLURL,
		HeadSHA: headSHA,
	}, nil
}

// maxRepoNameLength is GitHub's own repository name length limit.
const maxRepoNameLength = 100

// sanitizeRepoName maps an arbitrary display name to GitHub's allowed repo
// characters ([A-Za-z0-9._-]), collapsing any run of others to a single hyphen,
// trimming leading/trailing separators, and capping at GitHub's length limit so
// an over-long name fails as ErrInvalidRepoName here rather than surfacing as a
// GitHub 422 that classifyCreateRepoError would misreport as ErrRepoNameTaken.
// Empty when nothing usable remains.
func sanitizeRepoName(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	trimmed := strings.Trim(b.String(), "-._")
	if len(trimmed) > maxRepoNameLength {
		trimmed = strings.Trim(trimmed[:maxRepoNameLength], "-._")
	}
	return trimmed
}
