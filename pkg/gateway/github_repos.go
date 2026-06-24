package gateway

// GET /v1/github/repos — list the repositories the user's connected
// GitHub account can access, for the import-a-repo picker. Pure proxy to
// the host, where the GitHub token lives; the token never touches the
// gateway. See github_proxy.go for the shared plumbing.

import "net/http"

func (g *Gateway) handleGitHubListRepos(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/credentials/github/repos")
}
