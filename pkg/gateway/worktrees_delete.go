package gateway

import (
	"net/http"
	"net/url"
)

// handleDeleteWorktree services DELETE /v1/worktrees/{id} as a pure host
// proxy (repos_proxy.go style, verbatim status forwarding): the host
// purges the worktree's sessions and unlinks ~/work/{id} from its repo
// canonical. 204 on success, 409 (worktree_busy) while a session runs,
// 404-free idempotence for an already-gone worktree — all straight from
// the host. The host filesystem is the only registry; there is no
// gateway-side row to delete.
func (g *Gateway) handleDeleteWorktree(w http.ResponseWriter, r *http.Request) {
	worktreeID := r.PathValue("id")
	if worktreeID == "" {
		http.Error(w, "worktree id missing", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/worktrees/"+url.PathEscape(worktreeID))
}
