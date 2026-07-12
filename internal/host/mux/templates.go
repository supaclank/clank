package hostmux

import (
	"errors"
	"net/http"

	githubpkg "github.com/acksell/clank/internal/host/github"
)

// handleGitHubListTemplates services GET /templates/github — the
// user's own GitHub repositories marked as templates (is_template),
// with clone URLs. The gateway merges these with its builtin catalog
// on GET /v1/templates (stripping URLs toward clients) and resolves
// github: template ids against this same listing at create time.
//
// github_not_connected (409) is an expected state, not a failure: the
// gateway treats it as "no github templates" and the client may offer
// the GitHub Connect flow.
func (m *Mux) handleGitHubListTemplates(w http.ResponseWriter, r *http.Request) {
	g, ok := m.requireGitHub(w)
	if !ok {
		return
	}
	token, err := g.AccessToken()
	if err != nil {
		if errors.Is(err, githubpkg.ErrNotConnected) {
			writeJSON(w, http.StatusConflict, errResp{Code: "github_not_connected", Error: err.Error()})
			return
		}
		writeError(w, err)
		return
	}
	repos, err := g.ListTemplateRepositories(r.Context(), token)
	if err != nil {
		writeError(w, err)
		return
	}
	if repos == nil {
		repos = []githubpkg.TemplateRepo{}
	}
	writeJSON(w, http.StatusOK, repos)
}
