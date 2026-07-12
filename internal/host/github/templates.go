package github

// Template-repository support for the create-project flow: listing the
// user's own GitHub template repos (the "Template repository" checkbox
// on GitHub, is_template=true) and resolving one to a clone URL at
// create time. The host owns this — it holds the GitHub credential —
// so private template repos work and the gateway never sees tokens.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gogithub "github.com/google/go-github/v66/github"
)

// ErrNotTemplate reports that the referenced repository exists and is
// accessible but is not marked as a template on GitHub.
var ErrNotTemplate = errors.New("github: repository is not a template")

// ErrRepoNotFound reports that the referenced repository doesn't exist
// or the connected account can't see it — GitHub returns 404 for both,
// deliberately, so we don't distinguish either.
var ErrRepoNotFound = errors.New("github: repository not found or not accessible")

// ListTemplateRepositories lists repositories OWNED by the
// authenticated user that are marked as templates, most recently
// pushed first, capped at maxRepos. Owner-only affiliation is the v1
// product boundary: "your own templates" — org/community sources come
// later with their own trust story.
func (m *Manager) ListTemplateRepositories(ctx context.Context, token string) ([]Repo, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return nil, fmt.Errorf("build api client: %w", err)
	}

	opts := &gogithub.RepositoryListByAuthenticatedUserOptions{
		Affiliation: "owner",
		Sort:        "pushed",
		ListOptions: gogithub.ListOptions{PerPage: reposPerPage},
	}

	var out []Repo
	for {
		repos, resp, err := client.Repositories.ListByAuthenticatedUser(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list template repositories: %w", err)
		}
		for _, r := range repos {
			if !r.GetIsTemplate() {
				continue
			}
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

// ResolveTemplateRepo validates that owner/repo is an accessible
// template repository and returns its clone URL. The is_template check
// is the trust boundary: create-project only clones repos their owner
// deliberately published as templates.
func (m *Manager) ResolveTemplateRepo(ctx context.Context, token, owner, repo string) (cloneURL string, err error) {
	client, err := m.apiClient(token)
	if err != nil {
		return "", fmt.Errorf("build api client: %w", err)
	}
	r, resp, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("%w: %s/%s", ErrRepoNotFound, owner, repo)
		}
		return "", fmt.Errorf("get repository %s/%s: %w", owner, repo, err)
	}
	if !r.GetIsTemplate() {
		return "", fmt.Errorf("%w: %s/%s", ErrNotTemplate, owner, repo)
	}
	if r.GetCloneURL() == "" {
		return "", fmt.Errorf("repository %s/%s has no clone URL", owner, repo)
	}
	return r.GetCloneURL(), nil
}
