package github

// Repository creation — used by the "publish a greenfield app to GitHub"
// flow (POST /worktrees/{id}/remote/publish on the host). Wraps go-github's
// Repositories.Create so a remote-less worktree can get a fresh origin.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gogithub "github.com/google/go-github/v66/github"
)

// ErrRepoNameTaken is GitHub's 422 when the authenticated user already owns a
// repository with the requested name.
var ErrRepoNameTaken = errors.New("github: a repository with that name already exists on this account")

// CreateRepoInput is the request for CreateRepository. Creating a PRIVATE repo
// needs the token's `repo` scope — the connect flow already requests it.
type CreateRepoInput struct {
	Name        string
	Description string
	Private     bool
}

// CreatedRepo is the trimmed shape returned after a successful create.
type CreatedRepo struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	HTMLURL  string `json:"html_url"`
	CloneURL string `json:"clone_url"`
}

// CreateRepository creates a repository owned by the authenticated user (the
// empty org argument means "the token's user"). AutoInit is false on purpose:
// we push the worktree's existing history, so an initial commit on GitHub's
// side would make the first push a non-fast-forward.
func (m *Manager) CreateRepository(ctx context.Context, token string, in CreateRepoInput) (CreatedRepo, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return CreatedRepo{}, fmt.Errorf("build api client: %w", err)
	}
	req := &gogithub.Repository{
		Name:        gogithub.String(in.Name),
		Description: gogithub.String(in.Description),
		Private:     gogithub.Bool(in.Private),
		AutoInit:    gogithub.Bool(false),
	}
	repo, _, err := client.Repositories.Create(ctx, "", req)
	if err != nil {
		return CreatedRepo{}, classifyCreateRepoError(err)
	}
	return CreatedRepo{
		Owner:    repo.GetOwner().GetLogin(),
		Name:     repo.GetName(),
		FullName: repo.GetFullName(),
		Private:  repo.GetPrivate(),
		HTMLURL:  repo.GetHTMLURL(),
		CloneURL: repo.GetCloneURL(),
	}, nil
}

// classifyCreateRepoError maps GitHub's 422 "name already exists" to
// ErrRepoNameTaken; everything else wraps through.
func classifyCreateRepoError(err error) error {
	var er *gogithub.ErrorResponse
	if errors.As(err, &er) && er.Response.StatusCode == http.StatusUnprocessableEntity {
		for _, e := range er.Errors {
			if e.Field == "name" {
				return ErrRepoNameTaken
			}
		}
	}
	return fmt.Errorf("create repository: %w", err)
}
