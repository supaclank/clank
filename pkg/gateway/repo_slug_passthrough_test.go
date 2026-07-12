package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// repo_slug is how mobile's browse-first import navigates to
// /repo/{slug} straight from the create/import response. A gateway that
// decodes the host body into its own struct and re-encodes it will
// silently STRIP fields that struct lacks — exactly how repo_slug got
// lost once before. These tests pin the verbatim-passthrough contract.
func TestImportProject_RepoSlugPassesThrough(t *testing.T) {
	t.Parallel()
	const hostBody = `{"worktree_id":"01W","branch":"main","worktree_dir":"/w/01W","display_name":"api","origin_repo":"acme/api","repo_slug":"acme__api"}`
	mux := http.NewServeMux()
	mux.HandleFunc("POST /projects/import", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(hostBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gw := newProjectsGateway(t, srv)

	resp := postJSON(t, gw.URL+"/v1/projects/import", `{"owner":"acme","repo":"api"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"repo_slug":"acme__api"`) {
		t.Errorf("repo_slug stripped from import response: %s", body)
	}
}

func TestCreateProject_RepoSlugPassesThrough(t *testing.T) {
	t.Parallel()
	const hostBody = `{"worktree_id":"01W","branch":"main","worktree_dir":"/w/01W","display_name":"My App","origin_repo":"My App","repo_slug":"My-App"}`
	mux := http.NewServeMux()
	mux.HandleFunc("POST /projects/create", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(hostBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gw := newProjectsGateway(t, srv)

	resp := postJSON(t, gw.URL+"/v1/projects/create", `{"clone_url":"https://example.test/t.git","name":"My App"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"repo_slug":"My-App"`) {
		t.Errorf("repo_slug stripped from create response: %s", body)
	}
}
