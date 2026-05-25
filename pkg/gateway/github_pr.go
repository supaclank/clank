package gateway

// POST /v1/worktrees/{id}/pr — the marquee endpoint of the GitHub
// Connect feature. The gateway forwards the request body to the
// host at /worktrees/{id}/pr; the host pushes the branch and calls
// GitHub. See internal/host/github_pr.go for the orchestration.

import "net/http"

func (g *Gateway) handleGitHubCreatePR(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/worktrees/"+id+"/pr")
}

// handleGitHubPreviewPR proxies POST /v1/worktrees/{id}/pr/preview —
// the mobile sheet's "what destination will this go to" query.
// Mounted before the /v1/ catch-all so it doesn't fall into sync.
func (g *Gateway) handleGitHubPreviewPR(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/worktrees/"+id+"/pr/preview")
}
