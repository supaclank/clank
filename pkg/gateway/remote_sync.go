package gateway

// /v1/worktrees/{id}/remote/{status,push,pull,resolve} — proxied to the
// host's worktree↔GitHub-remote sync endpoints (internal/host/mux/remote.go).
// "Remote" is the git origin on GitHub — the only sync axis clank has.

import "net/http"

func (g *Gateway) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/worktrees/"+id+"/remote/status")
}

func (g *Gateway) handleRemotePush(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/worktrees/"+id+"/remote/push")
}

func (g *Gateway) handleRemotePull(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/worktrees/"+id+"/remote/pull")
}

func (g *Gateway) handleRemoteResolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/worktrees/"+id+"/remote/resolve")
}

func (g *Gateway) handleRemotePublish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id is required", http.StatusBadRequest)
		return
	}
	g.proxyHostGitHub(w, r, "/worktrees/"+id+"/remote/publish")
}
