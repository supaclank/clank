package gateway

// GET /v1/github/status — thin proxy to the host. The sprite owns
// the source-of-truth credential file, so we just forward the
// host's response shape. The gateway never caches; reads are cheap
// and avoid stale-cache bugs across disconnect/reconnect.

import "net/http"

func (g *Gateway) handleGitHubStatus(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/credentials/github/status")
}
