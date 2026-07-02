package host

// GET /repos — the repo + worktree listing derived straight from the
// filesystem (readdir + `git worktree list` per canonical). This is the
// replacement for the gateway-DB-backed GET /v1/worktrees: under the
// repo-first layout the host's disk IS the registry. Deliberately
// cheap — no per-branch history walks, no network. Per CLAUDE.md's
// per-method-file rule this operation lives on its own.

import (
	"context"
	"os"
	"path/filepath"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// RepoOrigin identifies a repo's GitHub origin. Nil on RepoInfo means
// the repo is unpublished greenfield — presence IS the "on GitHub" bit.
type RepoOrigin struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

// RepoWorktreeInfo is one loaded branch of a repo: the worktree's id
// (how sessions address it), its branch, and a display name (the
// branch — the only durable identity a worktree has host-side).
type RepoWorktreeInfo struct {
	WorktreeID  string `json:"worktree_id"`
	Branch      string `json:"branch"`
	DisplayName string `json:"display_name"`
}

// RepoInfo is one repo in the listing. Slug is the stable single-
// segment host-minted routing key (doubles as the dir name; NOT
// necessarily equal to owner/repo — greenfield repos are named before
// they have an origin, and a GitHub rename changes the label, not the
// slug).
type RepoInfo struct {
	Slug          string             `json:"slug"`
	Label         string             `json:"label"`
	Origin        *RepoOrigin        `json:"origin"`
	DefaultBranch string             `json:"default_branch"`
	Worktrees     []RepoWorktreeInfo `json:"worktrees"`
}

// ListRepos enumerates the canonical clones under ~/work/repos and
// their linked worktrees. Repos whose slug dir lacks a repo.git (torn
// creation) are skipped; worktree entries whose dir has vanished
// (manual rm — prunable bookkeeping) are filtered out.
func (s *Service) ListRepos(ctx context.Context) ([]RepoInfo, error) {
	root, err := reposRootDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []RepoInfo{}, nil // no repos yet
		}
		return nil, err
	}
	repos := make([]RepoInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slug := e.Name()
		gitDir := filepath.Join(root, slug, canonicalRepoDirName)
		if _, statErr := os.Stat(gitDir); statErr != nil {
			continue // torn creation or foreign dir — not a repo
		}
		info, err := s.repoInfoFor(slug, gitDir)
		if err != nil {
			// One broken canonical must not take the whole listing down.
			s.log.Printf("list repos: skip %s: %v", slug, err)
			continue
		}
		repos = append(repos, info)
	}
	return repos, nil
}

// repoInfoFor assembles one repo's listing entry from its canonical.
func (s *Service) repoInfoFor(slug, gitDir string) (RepoInfo, error) {
	defaultBranch, err := git.HeadBranch(gitDir)
	if err != nil {
		return RepoInfo{}, err
	}
	info := RepoInfo{
		Slug:          slug,
		Label:         repoLabelFor(gitDir),
		Origin:        repoOriginFor(gitDir),
		DefaultBranch: defaultBranch,
		Worktrees:     []RepoWorktreeInfo{},
	}
	worktrees, err := git.ListWorktrees(gitDir)
	if err != nil {
		return RepoInfo{}, err
	}
	for _, wt := range worktrees {
		if wt.Bare || wt.Branch == "" {
			continue
		}
		// Manual rm leaves prunable bookkeeping; a listing must reflect
		// what actually exists.
		if _, statErr := os.Stat(wt.Path); statErr != nil {
			continue
		}
		worktreeID, idErr := agent.ReadLocalWorktreeID(wt.Path)
		if idErr != nil || worktreeID == "" {
			s.log.Printf("list repos: %s worktree %s has no readable id: %v", slug, wt.Path, idErr)
			continue
		}
		info.Worktrees = append(info.Worktrees, RepoWorktreeInfo{
			WorktreeID:  worktreeID,
			Branch:      wt.Branch,
			DisplayName: wt.Branch,
		})
	}
	return info, nil
}

// repoOriginFor derives the nullable origin object from the canonical's
// origin remote. Non-GitHub or missing origins → nil (the "unpublished"
// signal).
func repoOriginFor(gitDir string) *RepoOrigin {
	remoteURL, err := git.RemoteURL(gitDir, "origin")
	if err != nil || remoteURL == "" {
		return nil
	}
	owner, repo, err := githubpkg.ParseGitHubRemote(remoteURL)
	if err != nil {
		return nil
	}
	return &RepoOrigin{Owner: owner, Repo: repo}
}
