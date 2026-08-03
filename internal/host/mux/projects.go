package hostmux

import (
	"errors"
	"net/http"

	githubpkg "github.com/supaclank/clank/internal/host/github"
)

// createProjectRequest is the body of POST /projects/create. The host is
// a generic executor here: it clones the concrete clone_url the gateway
// resolved from its template catalog (operator entries or the user's
// GitHub templates listed by GET /templates/github). The host never
// resolves template ids or accepts a client-supplied URL — the gateway
// is the gatekeeper.
type createProjectRequest struct {
	CloneURL string `json:"clone_url"`
	Name     string `json:"name"`
}

// HOST
func (m *Mux) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if req.CloneURL == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "clone_url is required"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "name is required"})
		return
	}
	// Clone with the most privileged credential available: the stored
	// GitHub token when connected (private template repos), nothing
	// otherwise. git only presents credentials when the server
	// challenges, so the token is inert for public clones.
	out, err := m.svc.CreateProjectFromTemplate(r.Context(), req.CloneURL, m.githubTokenIfConnected(), req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// githubTokenIfConnected returns the stored GitHub access token, or ""
// when GitHub isn't configured/connected — never an error: credentials
// are an upgrade for the clone, not a requirement.
func (m *Mux) githubTokenIfConnected() string {
	g := m.svc.GitHub()
	if g == nil {
		return ""
	}
	token, err := g.AccessToken()
	if err != nil {
		if !errors.Is(err, githubpkg.ErrNotConnected) {
			m.log.Printf("create-project: read github token: %v (cloning unauthenticated)", err)
		}
		return ""
	}
	return token
}
