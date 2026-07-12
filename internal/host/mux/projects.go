package hostmux

import (
	"errors"
	"net/http"
	"strings"

	githubpkg "github.com/acksell/clank/internal/host/github"
)

// createProjectRequest is the body of POST /projects/create. Exactly
// one source must be set:
//
//   - clone_url: a concrete git URL the gateway resolved from its
//     builtin template catalog. The host never resolves catalog ids or
//     accepts a client-supplied URL — the gateway is the gatekeeper.
//   - github_template: "owner/repo" naming one of the USER'S OWN
//     GitHub template repositories. The host resolves it with its
//     stored GitHub credential (validating is_template — the trust
//     boundary), so private templates work and the gateway never
//     touches tokens.
type createProjectRequest struct {
	CloneURL       string `json:"clone_url,omitempty"`
	GitHubTemplate string `json:"github_template,omitempty"`
	Name           string `json:"name"`
}

// HOST
func (m *Mux) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if (req.CloneURL == "") == (req.GitHubTemplate == "") {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_argument", Error: "exactly one of clone_url or github_template is required"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, errResp{Error: "name is required"})
		return
	}

	cloneURL := req.CloneURL
	if req.GitHubTemplate != "" {
		resolved, ok := m.resolveGitHubTemplate(w, r, req.GitHubTemplate)
		if !ok {
			return // response already written
		}
		cloneURL = resolved
	}

	out, err := m.svc.CreateProjectFromTemplate(r.Context(), cloneURL, req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// resolveGitHubTemplate turns an "owner/repo" template ref into a
// clone URL via the host's GitHub credential. On failure it writes the
// typed error response and returns ok=false.
func (m *Mux) resolveGitHubTemplate(w http.ResponseWriter, r *http.Request, ref string) (cloneURL string, ok bool) {
	owner, repo, valid := strings.Cut(ref, "/")
	if !valid || owner == "" || repo == "" || strings.Contains(repo, "/") {
		writeJSON(w, http.StatusBadRequest, errResp{Code: "invalid_argument", Error: "github_template must be \"owner/repo\""})
		return "", false
	}
	g, hasGitHub := m.requireGitHub(w)
	if !hasGitHub {
		return "", false
	}
	token, err := g.AccessToken()
	if err != nil {
		if errors.Is(err, githubpkg.ErrNotConnected) {
			writeJSON(w, http.StatusConflict, errResp{Code: "github_not_connected", Error: err.Error()})
			return "", false
		}
		writeError(w, err)
		return "", false
	}
	resolved, err := g.ResolveTemplateRepo(r.Context(), token, owner, repo)
	if err != nil {
		switch {
		case errors.Is(err, githubpkg.ErrRepoNotFound):
			writeJSON(w, http.StatusNotFound, errResp{Code: "template_not_found", Error: err.Error()})
		case errors.Is(err, githubpkg.ErrNotTemplate):
			writeJSON(w, http.StatusUnprocessableEntity, errResp{Code: "not_a_template", Error: err.Error()})
		default:
			writeError(w, err)
		}
		return "", false
	}
	return resolved, true
}
