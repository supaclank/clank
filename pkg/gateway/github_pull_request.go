package gateway

import (
	"net/http"
	"time"
)

const githubPullRequestLaunchTimeout = 10 * time.Minute

func (g *Gateway) handleGitHubPullRequestInspect(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/github/pull-requests/inspect")
}

func (g *Gateway) handleGitHubPullRequestLaunch(w http.ResponseWriter, r *http.Request) {
	g.proxyHost(w, r, "/github/pull-requests/launch", githubPullRequestLaunchTimeout)
}
