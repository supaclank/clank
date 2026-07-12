package github

// Template-repository listing for the create-project flow: the user's
// own GitHub repos marked as templates (the "Template repository"
// checkbox, is_template=true). The host owns this — it holds the
// GitHub credential — and returns clone URLs to the GATEWAY, which
// resolves template ids against them server-side; clients only ever
// see ids and display names.

import (
	"context"
	"fmt"

	gogithub "github.com/google/go-github/v66/github"
)

// TemplateRepo is one entry in the template listing: the trimmed repo
// shape plus the clone URL the gateway needs for create-time
// resolution. The clone URL never travels past the gateway.
type TemplateRepo struct {
	Repo
	CloneURL string `json:"clone_url"`
}

// ListTemplateRepositories lists repositories OWNED by the
// authenticated user that are marked as templates, most recently
// pushed first, capped at maxRepos. Owner-only affiliation is the v1
// product boundary: "your own templates" — org/community sources come
// later with their own trust story. Membership in this listing is
// also the create-time authorization: the gateway only creates from
// ids it can find here (or in the operator catalog).
func (m *Manager) ListTemplateRepositories(ctx context.Context, token string) ([]TemplateRepo, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return nil, fmt.Errorf("build api client: %w", err)
	}

	opts := &gogithub.RepositoryListByAuthenticatedUserOptions{
		Affiliation: "owner",
		Sort:        "pushed",
		ListOptions: gogithub.ListOptions{PerPage: reposPerPage},
	}

	var out []TemplateRepo
	for {
		repos, resp, err := client.Repositories.ListByAuthenticatedUser(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list template repositories: %w", err)
		}
		for _, r := range repos {
			if !r.GetIsTemplate() {
				continue
			}
			out = append(out, TemplateRepo{Repo: wireRepo(r), CloneURL: r.GetCloneURL()})
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
