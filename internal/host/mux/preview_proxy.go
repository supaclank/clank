package hostmux

import (
	"net/http"
)

// handlePreviewProxy is the catch-all that forwards everything under
// /worktrees/{id}/preview/proxy/... to the worktree's running dev
// server. Bound to ANY method (so HMR WebSocket upgrades flow through)
// and registered last in mux.go so it can't shadow the control-plane
// /preview/{start,stop,status} routes registered above it.
//
// The handler does NOT decode a body. ProxyHandler is responsible for
// stripping the route prefix so the dev server sees its own URL space
// rooted at "/".
//
// 404 with no_preview-style structured error is left to the underlying
// ProxyHandler (it calls http.Error directly with a hint). Clients hit
// this when they hold a stale URL after a sprite reboot.
func (m *Mux) handlePreviewProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "worktree id missing", http.StatusBadRequest)
		return
	}
	prefix := "/worktrees/" + id + "/preview/proxy"
	m.svc.PreviewProxyHandler(id, prefix).ServeHTTP(w, r)
}
