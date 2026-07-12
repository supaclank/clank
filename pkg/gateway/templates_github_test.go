package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// templatesHost stands in for the host's template surface: GET
// /templates/github plus a create endpoint recording the forwarded
// github_template ref.
type templatesHost struct {
	githubStatus      int    // status for GET /templates/github
	githubBody        string // body for GET /templates/github
	gotGitHubTemplate atomic.Value
	gotCloneURL       atomic.Value
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
			CloneURL       string `json:"clone_url"`
			GitHubTemplate string `json:"github_template"`
			Name           string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.gotGitHubTemplate.Store(body.GitHubTemplate)
		h.gotCloneURL.Store(body.CloneURL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"worktree_id": "01GHTPLWT", "display_name": body.Name})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

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
	h := &templatesHost{
		githubStatus: http.StatusOK,
		githubBody:   `[{"full_name":"acme/expo-tpl","description":"My starter"},{"full_name":"acme/go-tpl"}]`,
	}
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
	// Host that immediately closes: the picker must degrade, not fail.
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

func TestCreateProject_GitHubTemplateForwardsRef(t *testing.T) {
	t.Parallel()
	h := &templatesHost{githubStatus: http.StatusOK, githubBody: `[]`}
	gw := newProjectsGateway(t, h.server(t), nil)

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"github:acme/expo-tpl","name":"my-app"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := h.gotGitHubTemplate.Load(); got != "acme/expo-tpl" {
		t.Fatalf("host github_template = %v, want acme/expo-tpl", got)
	}
	if got := h.gotCloneURL.Load(); got != "" {
		t.Fatalf("host clone_url = %v, want empty (the host resolves github refs)", got)
	}
}

func TestCreateProject_GitHubTemplateMalformed(t *testing.T) {
	t.Parallel()
	h := &templatesHost{githubStatus: http.StatusOK, githubBody: `[]`}
	gw := newProjectsGateway(t, h.server(t), nil)

	for _, id := range []string{"github:", "github:acme", "github:acme/", "github:/tpl", "github:a/b/c"} {
		resp := postJSON(t, gw.URL+"/v1/projects/create", `{"template":"`+id+`","name":"x"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("template %q: status = %d, want 400", id, resp.StatusCode)
		}
	}
}
