package gateway

// Create-project surface: pure proxies. The HOST owns the whole
// template story — the builtin catalog arrives via its own config
// (clank-host --templates-json / $CLANK_TEMPLATES) and the user's
// GitHub templates via its stored credential — so self-hosted and
// laptop deployments work without gateway involvement. The gateway
// contributes only auth + routing, like every other host surface.

import (
	"net/http"
	"time"
)

// createProjectTimeout caps the host round-trip for project creation:
// cloning a template is a network operation on the host, so it gets
// far more headroom than a plain proxy call.
const createProjectTimeout = 120 * time.Second

// handleListTemplates proxies GET /v1/templates → host GET /templates:
// the full catalog (builtin + the user's GitHub template repos), each
// entry carrying the clone_url the client passes back to create.
func (g *Gateway) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	g.proxyHost(w, r, "/templates", hostGitHubTimeout)
}

// handleCreateProject proxies POST /v1/projects/create → host
// POST /projects/create. The host answers typed, sanitized errors
// (template_clone_failed etc.) that forward verbatim.
func (g *Gateway) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	g.proxyHost(w, r, "/projects/create", createProjectTimeout)
}
