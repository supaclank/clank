package hostmux_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
	githubpkg "github.com/supaclank/clank/internal/host/github"
	hostmux "github.com/supaclank/clank/internal/host/mux"
)

var builtinTemplates = []host.Template{
	{DisplayName: "Expo app", CloneURL: "https://templates.example/expo.git"},
}

// newTemplatesServer stands up a host with builtin templates and,
// when apiHandler is non-nil, a connected GitHub manager pointed at
// that stub API.
func newTemplatesServer(t *testing.T, apiHandler http.Handler) *httptest.Server {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	if apiHandler != nil {
		api := httptest.NewServer(apiHandler)
		t.Cleanup(api.Close)
		if err := githubpkg.NewStore(tmpHome).Write(githubpkg.Credentials{AccessToken: "gho_templates_test"}); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
		svc := host.New(host.Options{
			BackendManagers: map[agent.BackendType]agent.BackendManager{
				agent.BackendOpenCode: &noopBackendManager{},
			},
			Templates: builtinTemplates,
		})
		t.Cleanup(svc.Shutdown)
		svc.GitHub().SetAPIBaseURL(api.URL)
		srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
		t.Cleanup(srv.Close)
		return srv
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		Templates: builtinTemplates,
	})
	t.Cleanup(svc.Shutdown)
	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)
	return srv
}

type templateEntry struct {
	DisplayName string `json:"display_name"`
	CloneURL    string `json:"clone_url"`
	Source      string `json:"source"`
	Description string `json:"description"`
}

func fetchTemplates(t *testing.T, url string) []templateEntry {
	t.Helper()
	resp, body := mustGet(t, url+"/templates")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var got []templateEntry
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return got
}

func TestListTemplates_MergesBuiltinAndGitHub(t *testing.T) {
	srv := newTemplatesServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"tpl","full_name":"acme/tpl","is_template":true,"owner":{"login":"acme"},
			 "default_branch":"main","description":"My starter","clone_url":"https://github.example/acme/tpl.git"},
			{"name":"app","full_name":"acme/app","is_template":false,"owner":{"login":"acme"},"default_branch":"main"}
		]`))
	}))

	got := fetchTemplates(t, srv.URL)
	if len(got) != 2 {
		t.Fatalf("len = %d (%+v), want 2", len(got), got)
	}
	if got[0].Source != "builtin" || got[0].CloneURL != "https://templates.example/expo.git" {
		t.Errorf("builtin entry malformed: %+v", got[0])
	}
	if got[1].Source != "github" || got[1].CloneURL != "https://github.example/acme/tpl.git" || got[1].Description != "My starter" {
		t.Errorf("github entry malformed (is_template filter or fields): %+v", got[1])
	}
}

// GitHub not connected is a normal state: builtin entries still render.
func TestListTemplates_NotConnected_BuiltinOnly(t *testing.T) {
	srv := newTemplatesServer(t, nil)
	got := fetchTemplates(t, srv.URL)
	if len(got) != 1 || got[0].Source != "builtin" {
		t.Fatalf("want builtin-only, got %+v", got)
	}
}

// A GitHub API failure degrades to builtin-only instead of failing the
// picker.
func TestListTemplates_GitHubAPIDown_BuiltinOnly(t *testing.T) {
	srv := newTemplatesServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	got := fetchTemplates(t, srv.URL)
	if len(got) != 1 || got[0].Source != "builtin" {
		t.Fatalf("want builtin-only on API failure, got %+v", got)
	}
}

// TestCreateProject_CloneFailureIsTypedAndSanitized is the regression
// test for the opaque-502 incident: a template URL that can't be cloned
// (dev-stack .env pointed at a placeholder domain) surfaced as a host
// 500 whose body the gateway rightly withheld — leaving zero diagnostics
// anywhere. The host must now (a) answer a typed 422
// template_clone_failed, and (b) keep the URL out of the response body
// (clone URLs may embed credentials).
func TestCreateProject_CloneFailureIsTypedAndSanitized(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	srv := newTestServer(t)
	secretURL := "https://user:supersecret@template.invalid/repo.git"
	resp, err := http.Post(srv.URL+"/projects/create", "application/json",
		strings.NewReader(`{"clone_url":"`+secretURL+`","name":"doomed"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s, want 422", resp.StatusCode, buf.String())
	}
	if !strings.Contains(buf.String(), "template_clone_failed") {
		t.Fatalf("body %s lacks template_clone_failed code", buf.String())
	}
	if strings.Contains(buf.String(), "supersecret") || strings.Contains(buf.String(), "template.invalid") {
		t.Fatalf("response leaked clone URL details: %s", buf.String())
	}
}
