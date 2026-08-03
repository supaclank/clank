package gateway

import (
	"net/http"
	"time"
)

// TODO(ai-review): duplicated with clankcli's githubPullRequestLaunchTimeout; centralize if they need to diverge or a third copy appears. https://github.com/supaclank/clank/pull/217
const githubPullRequestLaunchTimeout = 10 * time.Minute

func (g *Gateway) handleGitHubPullRequestInspect(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/github/pull-requests/inspect")
}

func (g *Gateway) handleGitHubPullRequestLaunch(w http.ResponseWriter, r *http.Request) {
	g.proxyHost(w, r, "/github/pull-requests/launch", githubPullRequestLaunchTimeout)
}
