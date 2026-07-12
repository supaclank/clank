package hostmux_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
	hostmux "github.com/acksell/clank/internal/host/mux"
)

// newGitHubTemplatesFixture stands up a host whose GitHub manager is
// connected (seeded credential) and pointed at a stub GitHub API.
func newGitHubTemplatesFixture(t *testing.T, apiHandler http.Handler) *httptest.Server {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	api := httptest.NewServer(apiHandler)
	t.Cleanup(api.Close)

	if err := githubpkg.NewStore(tmpHome).Write(githubpkg.Credentials{AccessToken: "gho_templates_test"}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)
	svc.GitHub().SetAPIBaseURL(api.URL)

	srv := httptest.NewServer(hostmux.New(svc, nil).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestGitHubTemplates_ListsOwnTemplatesWithCloneURLs(t *testing.T) {
	srv := newGitHubTemplatesFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"tpl","full_name":"acme/tpl","is_template":true,"owner":{"login":"acme"},
			 "default_branch":"main","clone_url":"https://github.example/acme/tpl.git"},
			{"name":"app","full_name":"acme/app","is_template":false,"owner":{"login":"acme"},"default_branch":"main"}
		]`))
	}))

	resp, body := mustGet(t, srv.URL+"/templates/github")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var repos []githubpkg.TemplateRepo
	if err := json.Unmarshal(body, &repos); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/tpl" {
		t.Fatalf("repos = %+v, want only acme/tpl", repos)
	}
	// The gateway resolves github: template ids against this clone_url.
	if repos[0].CloneURL != "https://github.example/acme/tpl.git" {
		t.Fatalf("clone_url = %q", repos[0].CloneURL)
	}
}

func TestGitHubTemplates_NotConnected(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	srv := newTestServer(t)
	resp, body := mustGet(t, srv.URL+"/templates/github")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d body=%s, want 409", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "github_not_connected") {
		t.Fatalf("body %s lacks github_not_connected code", body)
	}
}

// TestCreateProject_WorksWithGitHubConnected pins the "most privileged
// credentials available" behavior: with a credential stored, create
// still succeeds for a template that never challenges for auth (the
// token is inert unless the server asks).
func TestCreateProject_WorksWithGitHubConnected(t *testing.T) {
	tpl := templateCloneURL(t)
	srv := newGitHubTemplatesFixture(t, http.NotFoundHandler())

	resp, err := http.Post(srv.URL+"/projects/create", "application/json",
		strings.NewReader(`{"clone_url":"`+tpl+`","name":"from-tpl"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%s", resp.StatusCode, buf.String())
	}
	var out host.CreateWorktreeResult
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.WorktreeID == "" || out.DisplayName != "from-tpl" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

// TestCreateProject_CloneFailureIsTypedAndSanitized is the regression
// test for the opaque-502 incident: a template URL that can't be cloned
// (dev-stack .env pointed at a placeholder domain) surfaced as a host
// 500 whose body the gateway rightly withheld — leaving zero diagnostics
// anywhere. The host must now (a) answer a typed 422
// template_clone_failed the gateway can forward, and (b) keep the URL
// out of the response body (clone URLs may embed credentials).
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
