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

func TestGitHubTemplates_ListsOwnTemplates(t *testing.T) {
	srv := newGitHubTemplatesFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"tpl","full_name":"acme/tpl","is_template":true,"owner":{"login":"acme"},"default_branch":"main"},
			{"name":"app","full_name":"acme/app","is_template":false,"owner":{"login":"acme"},"default_branch":"main"}
		]`))
	}))

	resp, body := mustGet(t, srv.URL+"/templates/github")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var repos []githubpkg.Repo
	if err := json.Unmarshal(body, &repos); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/tpl" {
		t.Fatalf("repos = %+v, want only acme/tpl", repos)
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

// TestCreateProject_GitHubTemplate drives the full flow: the handler
// resolves owner/repo through the (stub) GitHub API — asserting
// is_template — then clones the resolved URL, which points at a REAL
// local template repo, and scaffolds the project.
func TestCreateProject_GitHubTemplate(t *testing.T) {
	// The fixture's tmp HOME scopes ~/work, so scaffolding stays in the
	// test sandbox.
	tpl := templateCloneURL(t)

	srv := newGitHubTemplatesFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/tpl":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "tpl", "full_name": "acme/tpl", "is_template": true,
				"clone_url": tpl, "owner": map[string]string{"login": "acme"},
			})
		case "/repos/acme/notatpl":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "notatpl", "full_name": "acme/notatpl", "is_template": false,
				"clone_url": tpl, "owner": map[string]string{"login": "acme"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))

	post := func(body string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/projects/create", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp, buf.Bytes()
	}

	// Happy path: template resolved + cloned.
	resp, body := post(`{"github_template":"acme/tpl","name":"from-my-template"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var out host.CreateWorktreeResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.WorktreeID == "" || out.DisplayName != "from-my-template" {
		t.Fatalf("unexpected result: %+v", out)
	}

	// Not marked as a template → typed 422.
	resp, body = post(`{"github_template":"acme/notatpl","name":"x"}`)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "not_a_template") {
		t.Fatalf("non-template: status=%d body=%s, want 422 not_a_template", resp.StatusCode, body)
	}

	// Unknown repo → typed 404.
	resp, body = post(`{"github_template":"acme/ghost","name":"x"}`)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "template_not_found") {
		t.Fatalf("missing repo: status=%d body=%s, want 404 template_not_found", resp.StatusCode, body)
	}

	// Both sources (or neither) rejected.
	resp, body = post(`{"github_template":"acme/tpl","clone_url":"https://x.test/y.git","name":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("both sources: status=%d body=%s, want 400", resp.StatusCode, body)
	}
	resp, body = post(`{"name":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("neither source: status=%d body=%s, want 400", resp.StatusCode, body)
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
