package hostmux

import (
	"errors"
	"net/http"

	githubpkg "github.com/supaclank/clank/internal/host/github"
)

// Template sources. Builtin entries come from operator config
// (clank-host --templates-json / $CLANK_TEMPLATES); github entries are
// the user's own template repos. Future sources (community, enterprise
// org) extend this — clients should tolerate unknown values.
const (
	templateSourceBuiltin = "builtin"
	templateSourceGitHub  = "github"
)

// templateEntry is one create-project catalog entry. A template's
// identity IS its clone URL: clients pick an entry and pass clone_url
// straight to POST /projects/create.
type templateEntry struct {
	DisplayName string `json:"display_name"`
	CloneURL    string `json:"clone_url"`
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
}

// handleListTemplates services GET /templates — the full create-project
// catalog: operator-configured builtin entries plus, when GitHub is
// connected, the user's own repositories marked as templates
// (is_template). "GitHub not connected" is a normal state, not an
// error: the builtin half still renders and clients may offer the
// GitHub Connect flow. A GitHub API failure degrades to builtin-only
// (logged) rather than failing the picker.
func (m *Mux) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	entries := make([]templateEntry, 0, len(m.svc.Templates()))
	for _, t := range m.svc.Templates() {
		entries = append(entries, templateEntry{
			DisplayName: t.DisplayName,
			CloneURL:    t.CloneURL,
			Source:      templateSourceBuiltin,
		})
	}
	entries = append(entries, m.githubTemplateEntries(r)...)
	writeJSON(w, http.StatusOK, entries)
}

// githubTemplateEntries lists the user's own GitHub template repos,
// best-effort: unconfigured manager, not-connected, and API failures
// all yield nil so the builtin catalog always renders.
func (m *Mux) githubTemplateEntries(r *http.Request) []templateEntry {
	g := m.svc.GitHub()
	if g == nil {
		return nil
	}
	token, err := g.AccessToken()
	if err != nil {
		if !errors.Is(err, githubpkg.ErrNotConnected) {
			m.log.Printf("templates: read github token: %v (builtin-only)", err)
		}
		return nil
	}
	repos, err := g.ListTemplateRepositories(r.Context(), token)
	if err != nil {
		m.log.Printf("templates: list github templates: %v (builtin-only)", err)
		return nil
	}
	entries := make([]templateEntry, 0, len(repos))
	for _, repo := range repos {
		entries = append(entries, templateEntry{
			DisplayName: repo.FullName,
			CloneURL:    repo.CloneURL,
			Source:      templateSourceGitHub,
			Description: repo.Description,
		})
	}
	return entries
}
