package gateway

// GET /v1/github/repos — list the repositories the user's connected
// GitHub account can access, for the import-a-repo picker. Pure proxy to
// the host, where the GitHub token lives; the token never touches the
// gateway. See github_proxy.go for the shared plumbing.

import (
	"net/http"
	"net/url"
)

func (g *Gateway) handleGitHubListRepos(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/credentials/github/repos")
}

// handleGitHubListBranches proxies GET
// /v1/github/repos/{owner}/{repo}/branches to the host's
// /credentials/github/repos/{owner}/{repo}/branches — the branch picker
// for the import flow. owner/repo are re-escaped into the forwarded path;
// the host re-validates them.
func (g *Gateway) handleGitHubListBranches(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if owner == "" || repo == "" {
		http.Error(w, "owner and repo are required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/credentials/github/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/branches")
}

// handleGitHubListPulls proxies GET /v1/github/repos/{owner}/{repo}/pulls to
// the host's /credentials/github/repos/{owner}/{repo}/pulls — the repo-detail
// screen's open-PR list. Mirrors handleGitHubListBranches.
func (g *Gateway) handleGitHubListPulls(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if owner == "" || repo == "" {
		http.Error(w, "owner and repo are required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/credentials/github/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/pulls")
}
