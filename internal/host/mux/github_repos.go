package hostmux

// GET /credentials/github/repos — list the repositories the connected
// GitHub account can access, for the "import an existing repo" picker.
// The token never leaves the host; the gateway is a pure proxy.

import (
	"net/http"

	githubpkg "github.com/acksell/clank/internal/host/github"
)

// listReposResponse wraps the repo slice so the shape can grow (cursors,
// counts) without breaking clients that already decode an object.
type listReposResponse struct {
	Repos []githubpkg.Repo `json:"repos"`
}

func (m *Mux) handleGitHubListRepos(w http.ResponseWriter, r *http.Request) {
	g, ok := m.requireGitHub(w)
	if !ok {
		return
	}
	token, err := g.AccessToken()
	if err != nil {
		writeGitHubFlowErr(w, err)
		return
	}
	repos, err := g.ListRepositories(r.Context(), token)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listReposResponse{Repos: repos})
}
