package gateway

import (
	"encoding/json"
	"net/http"
)

// Template is one entry in the project-creation catalog. ID is the
// stable handle clients send to POST /v1/projects/create; DisplayName is
// the human label for the picker; CloneURL is the git URL the host
// clones. The operator supplies these at deploy time (Config.Templates)
// — nothing here is hardcoded in OSS, keeping the gateway brand-agnostic.
type Template struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CloneURL    string `json:"clone_url"`
}

// templateSummary is the client-facing view of a Template. CloneURL is
// deliberately omitted — the catalog never leaks template source URLs to
// clients; the gateway resolves them server-side.
type templateSummary struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// cloneURLForTemplate resolves a template id to its clone URL. The
// second return is false when no template with that id is configured.
func (g *Gateway) cloneURLForTemplate(id string) (string, bool) {
	for _, t := range g.cfg.Templates {
		if t.ID == id {
			return t.CloneURL, true
		}
	}
	return "", false
}

// handleListTemplates serves GET /v1/templates — the catalog the
// create-project picker reads. Returns an empty array (never null) when
// no templates are configured.
func (g *Gateway) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	out := make([]templateSummary, 0, len(g.cfg.Templates))
	for _, t := range g.cfg.Templates {
		out = append(out, templateSummary{ID: t.ID, DisplayName: t.DisplayName})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
