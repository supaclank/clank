package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// templatesHost stands in for the host's template surface: GET
// /templates/github (entries with clone URLs) plus a create endpoint
// recording the clone_url the gateway resolved.
type templatesHost struct {
	githubStatus int    // status for GET /templates/github
	githubBody   string // body for GET /templates/github
	gotCloneURL  atomic.Value
}

func (h *templatesHost) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /templates/github", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(h.githubStatus)
		_, _ = w.Write([]byte(h.githubBody))
	})
	mux.HandleFunc("POST /projects/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			CloneURL string `json:"clone_url"`
			Name     string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.gotCloneURL.Store(body.CloneURL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"worktree_id": "01GHTPLWT", "display_name": body.Name})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const githubCatalogBody = `[
	{"full_name":"acme/expo-tpl","description":"My starter","clone_url":"https://github.example/acme/expo-tpl.git"},
	{"full_name":"acme/go-tpl","clone_url":"https://github.example/acme/go-tpl.git"}
]`

func getTemplates(t *testing.T, gwURL string) []templateSummary {
	t.Helper()
	resp, err := http.Get(gwURL + "/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var out []templateSummary
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return out
}

func TestListTemplates_MergesGitHubTemplates(t *testing.T) {
	t.Parallel()
	h := &templatesHost{githubStatus: http.StatusOK, githubBody: githubCatalogBody}
	gw := newProjectsGateway(t, h.server(t), []Template{
		{ID: "expo", DisplayName: "Expo app", CloneURL: "https://example.test/expo.git"},
	})

	got := getTemplates(t, gw.URL)
	if len(got) != 3 {
		t.Fatalf("len = %d (%+v), want 3", len(got), got)
	}
	if got[0].ID != "expo" || got[0].Source != "builtin" {
		t.Errorf("builtin entry malformed: %+v", got[0])
	}
	if got[1].ID != "github:acme/expo-tpl" || got[1].Source != "github" || got[1].Description != "My starter" {
		t.Errorf("github entry malformed: %+v", got[1])
	}
}

// TestListTemplates_NeverLeaksCloneURLs pins the client-facing
// invariant: the host hands the gateway clone URLs for resolution, and
// they must stop there.
func TestListTemplates_NeverLeaksCloneURLs(t *testing.T) {
	t.Parallel()
	h := &templatesHost{githubStatus: http.StatusOK, githubBody: githubCatalogBody}
	gw := newProjectsGateway(t, h.server(t), []Template{
		{ID: "expo", DisplayName: "Expo app", CloneURL: "https://example.test/expo.git"},
	})

	resp, err := http.Get(gw.URL + "/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, secret := range []string{"github.example", "example.test", "clone_url"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("clone URL material %q leaked to clients: %s", secret, body)
		}
	}
}

func TestListTemplates_GitHubNotConnected_BuiltinOnly(t *testing.T) {
	t.Parallel()
	h := &templatesHost{
		githubStatus: http.StatusConflict,
		githubBody:   `{"code":"github_not_connected","error":"github: not connected"}`,
	}
	gw := newProjectsGateway(t, h.server(t), []Template{
		{ID: "expo", DisplayName: "Expo app", CloneURL: "https://example.test/expo.git"},
	})

	got := getTemplates(t, gw.URL)
	if len(got) != 1 || got[0].ID != "expo" {
		t.Fatalf("want builtin-only, got %+v", got)
	}
}

func TestListTemplates_HostUnreachable_BuiltinOnly(t *testing.T) {
	t.Parallel()
	// Host that immediately aborts: the picker must degrade, not fail.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(dead.Close)
	gw := newProjectsGateway(t, dead, []Template{
		{ID: "expo", DisplayName: "Expo app", CloneURL: "https://example.test/expo.git"},
	})

	got := getTemplates(t, gw.URL)
	if len(got) != 1 || got[0].ID != "expo" {
		t.Fatalf("want builtin-only on host failure, got %+v", got)
	}
}

// TestCreateProject_GitHubTemplateResolvedFromListing: a github: id is
// resolved by membership in the user's own template listing — the host
// receives the listing's clone_url, exactly like a builtin resolution.
func TestCreateProject_GitHubTemplateResolvedFromListing(t *testing.T) {
	t.Parallel()
	h := &templatesHost{githubStatus: http.StatusOK, githubBody: githubCatalogBody}
	gw := newProjectsGateway(t, h.server(t), nil)

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"github:acme/expo-tpl","name":"my-app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := h.gotCloneURL.Load(); got != "https://github.example/acme/expo-tpl.git" {
		t.Fatalf("host clone_url = %v, want the listing's clone URL", got)
	}
}

// TestCreateProject_GitHubTemplateNotInListing: ids outside the user's
// own listing 404 — deleted repos, un-flagged templates, guessed names
// and malformed refs are all indistinguishable from never-existed.
func TestCreateProject_GitHubTemplateNotInListing(t *testing.T) {
	t.Parallel()
	h := &templatesHost{githubStatus: http.StatusOK, githubBody: githubCatalogBody}
	gw := newProjectsGateway(t, h.server(t), nil)

	for _, id := range []string{"github:acme/ghost", "github:", "github:acme", "github:a/b/c"} {
		resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"`+id+`","name":"x"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("template %q: status = %d, want 404", id, resp.StatusCode)
		}
	}
}
