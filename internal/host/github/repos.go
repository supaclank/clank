package github

// Repository listing for the "import an existing GitHub repo" flow.
// Wraps go-github's Repositories.ListByAuthenticatedUser and trims the
// response to the fields clients need, so go-github types don't leak
// across the package boundary (same approach as wirePR in pr.go).

import (
	"context"
	"errors"
	"fmt"
	"time"

	gogithub "github.com/google/go-github/v66/github"
)

// ErrNotConnected reports that no GitHub credential is stored on this
// host — the user hasn't completed the device flow. Distinct from
// ErrNotConfigured (the host has no OAuth client_id at all).
var ErrNotConnected = errors.New("github: not connected")

// maxRepos caps how many repositories ListRepositories returns. Picking
// from a list is a human action; an unbounded fetch on accounts with
// thousands of repos would be slow and pointless for the UI.
const maxRepos = 300

// reposPerPage is the page size for the listing pagination.
const reposPerPage = 100

// Repo is the trimmed repository shape returned to clients. FullName is
// "owner/name"; Owner/Name are split out so the import endpoint can take
// them as discrete fields without re-parsing.
type Repo struct {
	Owner         string    `json:"owner"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Private       bool      `json:"private"`
	DefaultBranch string    `json:"default_branch"`
	UpdatedAt     time.Time `json:"updated_at,omitzero"`
}

// AccessToken returns the stored GitHub access token, or ErrNotConnected
// when none is present. Centralizes the "connected?" check so callers
// (repo listing, import) don't re-implement it.
func (m *Manager) AccessToken() (string, error) {
	c, err := m.store.Read()
	if err != nil {
		return "", err
	}
	if c.AccessToken == "" {
		return "", ErrNotConnected
	}
	return c.AccessToken, nil
}

// ListRepositories lists repositories the authenticated user can access
// (owner, collaborator, and org-member affiliations), most recently
// pushed first, capped at maxRepos. token authenticates the call.
func (m *Manager) ListRepositories(ctx context.Context, token string) ([]Repo, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return nil, fmt.Errorf("build api client: %w", err)
	}

	opts := &gogithub.RepositoryListByAuthenticatedUserOptions{
		Sort:        "pushed",
		ListOptions: gogithub.ListOptions{PerPage: reposPerPage},
	}

	var out []Repo
	for {
		repos, resp, err := client.Repositories.ListByAuthenticatedUser(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list repositories: %w", err)
		}
		for _, r := range repos {
			out = append(out, wireRepo(r))
			if len(out) >= maxRepos {
				return out, nil
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// wireRepo collapses go-github's Repository to the trimmed Repo type.
func wireRepo(r *gogithub.Repository) Repo {
	return Repo{
		Owner:         r.GetOwner().GetLogin(),
		Name:          r.GetName(),
		FullName:      r.GetFullName(),
		Private:       r.GetPrivate(),
		DefaultBranch: r.GetDefaultBranch(),
		UpdatedAt:     r.GetUpdatedAt().Time,
	}
}

// Branch is the trimmed branch shape returned to clients for the import
// branch picker. Protected mirrors GitHub's branch-protection flag so the
// UI can badge protected branches; the picker marks the repo's default
// using the default_branch already carried by the repos list.
type Branch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
}

// maxBranches caps how many branches ListBranches returns. As with repos,
// picking is a human action — an unbounded fetch on repos with thousands
// of branches would be slow and pointless for the UI.
const maxBranches = 300

// branchesPerPage is the page size for the branch listing pagination.
const branchesPerPage = 100

// ListBranches lists the branches of owner/repo as returned by GitHub,
// capped at maxBranches. token authenticates the call (private repos need
// it). Mirrors ListRepositories' paginate-and-trim approach.
func (m *Manager) ListBranches(ctx context.Context, token, owner, repo string) ([]Branch, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return nil, fmt.Errorf("build api client: %w", err)
	}

	opts := &gogithub.BranchListOptions{
		ListOptions: gogithub.ListOptions{PerPage: branchesPerPage},
	}

	var out []Branch
	for {
		branches, resp, err := client.Repositories.ListBranches(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list branches: %w", err)
		}
		for _, b := range branches {
			out = append(out, Branch{Name: b.GetName(), Protected: b.GetProtected()})
			if len(out) >= maxBranches {
				return out, nil
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}
