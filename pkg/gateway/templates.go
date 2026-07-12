package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/acksell/clank/pkg/auth"
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

// Template sources surfaced to clients. Builtin entries come from the
// operator catalog; github entries are the user's own template repos,
// listed live by their host. Future sources (community, enterprise
// org) extend this enum — clients should tolerate unknown values.
const (
	templateSourceBuiltin = "builtin"
	templateSourceGitHub  = "github"
)

// githubTemplateIDPrefix namespaces user-template ids so the create
// endpoint can route them without a catalog lookup: "github:owner/repo".
const githubTemplateIDPrefix = "github:"

// templateSummary is the client-facing view of a template. CloneURL is
// deliberately omitted — the catalog never leaks template source URLs to
// clients; resolution happens server-side (gateway for builtin, host
// for github).
type templateSummary struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
}

// hostTemplateRepo mirrors the host's GET /templates/github entry
// (internal/host/github.Repo) — only the fields the picker needs.
type hostTemplateRepo struct {
	FullName    string `json:"full_name"`
	Description string `json:"description"`
}

// githubTemplatesTimeout caps the host call on the picker path. The
// picker must stay usable when the host is cold or GitHub is slow —
// builtin entries render regardless.
const githubTemplatesTimeout = 8 * time.Second

// cloneURLForTemplate resolves a builtin template id to its clone URL.
// The second return is false when no template with that id is
// configured (github: ids are resolved by the host, not here).
func (g *Gateway) cloneURLForTemplate(id string) (string, bool) {
	for _, t := range g.cfg.Templates {
		if t.ID == id {
			return t.CloneURL, true
		}
	}
	return "", false
}

// handleListTemplates serves GET /v1/templates — the catalog the
// create-project picker reads: the operator's builtin templates plus
// the user's own GitHub template repos (source:"github"), merged
// best-effort. Returns an empty array (never null) when nothing is
// available. Host/GitHub failures degrade to builtin-only rather than
// failing the picker.
func (g *Gateway) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	out := make([]templateSummary, 0, len(g.cfg.Templates))
	for _, t := range g.cfg.Templates {
		out = append(out, templateSummary{ID: t.ID, DisplayName: t.DisplayName, Source: templateSourceBuiltin})
	}
	out = append(out, g.githubTemplatesForUser(r.Context(), principal.UserID)...)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// githubTemplatesForUser fetches the user's GitHub template repos from
// their host. Best-effort by design: "github not connected" and any
// transport/host failure both yield nil (logged for the latter) so the
// builtin catalog always renders. Note this wakes the user's host —
// acceptable on the create-project path, where the host is about to be
// needed anyway.
func (g *Gateway) githubTemplatesForUser(ctx context.Context, userID string) []templateSummary {
	ctx, cancel := context.WithTimeout(ctx, githubTemplatesTimeout)
	defer cancel()

	ref, err := g.cfg.Provisioner.EnsureHost(ctx, userID)
	if err != nil {
		g.log.Printf("gateway templates: ensure host: %v (builtin-only)", err)
		return nil
	}
	target := strings.TrimRight(ref.URL, "/") + "/templates/github"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		g.log.Printf("gateway templates: build host request: %v (builtin-only)", err)
		return nil
	}
	cli := &http.Client{Transport: ref.Transport, Timeout: githubTemplatesTimeout}
	resp, err := cli.Do(req)
	if err != nil {
		g.log.Printf("gateway templates: host call: %v (builtin-only)", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		// github_not_connected — an expected state, not an error.
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		g.log.Printf("gateway templates: host returned %d (builtin-only)", resp.StatusCode)
		return nil
	}
	var repos []hostTemplateRepo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&repos); err != nil {
		g.log.Printf("gateway templates: decode host response: %v (builtin-only)", err)
		return nil
	}
	out := make([]templateSummary, 0, len(repos))
	for _, r := range repos {
		out = append(out, templateSummary{
			ID:          githubTemplateIDPrefix + r.FullName,
			DisplayName: r.FullName,
			Source:      templateSourceGitHub,
			Description: r.Description,
		})
	}
	return out
}

// githubTemplateRef extracts and validates the "owner/repo" ref from a
// github-namespaced template id. Returns ok=false (with a reason) for
// anything malformed — the gateway validates shape, the host validates
// existence and template-ness.
func githubTemplateRef(id string) (ref string, err error) {
	ref = strings.TrimPrefix(id, githubTemplateIDPrefix)
	owner, repo, found := strings.Cut(ref, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("malformed github template id %q (want github:owner/repo)", id)
	}
	return ref, nil
}
