package gateway

// DELETE /v1/github — removes the GitHub credential file on the
// user's host. Idempotent (host's DELETE returns 204 even when no
// file exists), so retries from the client are safe.

import "net/http"

func (g *Gateway) handleGitHubDisconnect(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/credentials/github")
}
