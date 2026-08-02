package gateway

import (
	"net/http"
	"time"
)

const githubRepositoryLaunchTimeout = 10 * time.Minute

func (g *Gateway) handleGitHubRepositoryInspect(w http.ResponseWriter, r *http.Request) {
	g.proxyHostGitHub(w, r, "/github/repositories/inspect")
}

func (g *Gateway) handleGitHubRepositoryLaunch(w http.ResponseWriter, r *http.Request) {
	g.proxyHost(w, r, "/github/repositories/launch", githubRepositoryLaunchTimeout)
}
