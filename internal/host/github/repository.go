package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gogithub "github.com/google/go-github/v66/github"
)

var (
	ErrRepositoryNotFound     = errors.New("github: repository not found")
	ErrRepositoryForbidden    = errors.New("github: repository forbidden")
	ErrRepositoryTokenInvalid = errors.New("github: repository token invalid")
)

// RepositoryDetails is the repository metadata needed before Clank imports code.
type RepositoryDetails struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	HTMLURL       string `json:"html_url"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	IsPrivate     bool   `json:"is_private"`
}

// GetRepository returns one repository. An empty token deliberately makes an
// anonymous request so public repositories do not require a GitHub connection.
func (m *Manager) GetRepository(ctx context.Context, token, owner, repo string) (RepositoryDetails, error) {
	client, err := m.apiClient(token)
	if err != nil {
		return RepositoryDetails{}, fmt.Errorf("build api client: %w", err)
	}
	repository, resp, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusNotFound:
				return RepositoryDetails{}, fmt.Errorf("%w: %s/%s", ErrRepositoryNotFound, owner, repo)
			case http.StatusUnauthorized:
				return RepositoryDetails{}, ErrRepositoryTokenInvalid
			case http.StatusForbidden:
				return RepositoryDetails{}, ErrRepositoryForbidden
			}
		}
		return RepositoryDetails{}, fmt.Errorf("get repository: %w", err)
	}
	return wireRepository(repository), nil
}

func wireRepository(repository *gogithub.Repository) RepositoryDetails {
	return RepositoryDetails{
		Owner:         repository.GetOwner().GetLogin(),
		Name:          repository.GetName(),
		HTMLURL:       repository.GetHTMLURL(),
		Description:   repository.GetDescription(),
		DefaultBranch: repository.GetDefaultBranch(),
		IsPrivate:     repository.GetPrivate(),
	}
}
