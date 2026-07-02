package gateway

// /v1/repos* — the repo-first surface, proxied verbatim to the host
// (internal/host/mux/repos.go). Pure proxies: the host owns all state
// (its filesystem IS the repo registry), and STATUS CODES ARE FORWARDED
// UNTOUCHED — the 502-masking that plagued /v1/worktrees/create (any
// host non-2xx flattened to a gateway 502) has no equivalent here.
// Mounted before the `/v1/` sync catch-all, like the other specific
// routes. proxyHostGitHub is the general host proxy despite its
// historical name (EnsureHost + body/status/content-type forwarding).

import (
	"net/http"
	"net/url"
)

// handleReposList proxies GET /v1/repos → host GET /repos: the
// filesystem-derived repo + worktree listing that replaces the
// sync-DB-backed GET /v1/worktrees at the mobile cutover.
func (g *Gateway) handleReposList(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/repos")
}

// handleRepoWorktreeCreate proxies POST /v1/repos/{slug}/worktrees —
// repo-scoped worktree creation (fork via base_branch / load via
// branch). Replaces POST /v1/worktrees/create.
func (g *Gateway) handleRepoWorktreeCreate(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		http.Error(w, "repo slug is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/repos/"+url.PathEscape(slug)+"/worktrees")
}

// handleRepoOverview proxies GET /v1/repos/{slug}/overview — the branch
// ∪ open-PR feed. The ?fetch=1 query rides through on RawQuery.
// TODO(ai-review): ?fetch=1 can run an unbounded host git fetch under
// proxyHostGitHub's fixed hostGitHubTimeout. https://github.com/Acksell/clank/pull/97#discussion_r3512816237
func (g *Gateway) handleRepoOverview(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		http.Error(w, "repo slug is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/repos/"+url.PathEscape(slug)+"/overview")
}

// handleRepoDelete proxies DELETE /v1/repos/{slug} — whole-repo removal
// (worktrees + sessions + canonical) with the host's busy guard.
func (g *Gateway) handleRepoDelete(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		http.Error(w, "repo slug is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/repos/"+url.PathEscape(slug))
}
