package gateway

// /v1/github/connect/* — three proxies driving the device-flow
// state machine that lives on the user's host. The connect flow
// itself never touches the gateway: mobile/TUI opens a browser to
// the verification URL the host returned, and the host polls GitHub
// directly. We just pass the start/status/cancel calls through.

import "net/http"

func (g *Gateway) handleGitHubConnectStart(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/credentials/github/connect/start")
}

func (g *Gateway) handleGitHubConnectStatus(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/credentials/github/connect/status")
}

func (g *Gateway) handleGitHubConnectCancel(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/credentials/github/connect/cancel")
}
