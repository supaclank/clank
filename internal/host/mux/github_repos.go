package hostmux

// GET /credentials/github/repos — list the repositories the connected
// GitHub account can access, for the "import an existing repo" picker.
// The token never leaves the host; the gateway is a pure proxy.

import (
	"net/http"
	"regexp"

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

// listBranchesResponse wraps the branch slice for the same growth reason
// as listReposResponse.
type listBranchesResponse struct {
	Branches []githubpkg.Branch `json:"branches"`
}

// gitHubNameRe accepts the subset of owner/repo names allowed by GitHub:
// alphanumeric, dots, hyphens, and underscores.
var gitHubNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func isValidGitHubName(s string) bool {
	return gitHubNameRe.MatchString(s)
}

// handleGitHubListBranches services GET
// /credentials/github/repos/{owner}/{repo}/branches — the branch picker
// for the import flow. Like list-repos, the token never leaves the host.
func (m *Mux) handleGitHubListBranches(w http.ResponseWriter, r *http.Request) {
	g, ok := m.requireGitHub(w)
	if !ok {
		return
	}
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if owner == "" || repo == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "owner and repo are required"})
		return
	}
	if !isValidGitHubName(owner) || !isValidGitHubName(repo) {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "bad_request", Error: "invalid owner or repo name"})
		return
	}
	token, err := g.AccessToken()
	if err != nil {
		writeGitHubFlowErr(w, err)
		return
	}
	branches, err := g.ListBranches(r.Context(), token, owner, repo)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listBranchesResponse{Branches: branches})
}
