package daemonclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubPullRequestInspectAndLaunch(t *testing.T) {
	t.Parallel()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["owner"] != "acme" || body["repo"] != "api" || body["number"] != float64(7) {
			t.Errorf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/github/pull-requests/inspect":
			_, _ = w.Write([]byte(`{"owner":"acme","repo":"api","number":7,"head_owner":"contributor","head_repo":"api-fork","head_sha":"abc"}`))
		case "/v1/github/pull-requests/launch":
			if body["expected_head_sha"] != "abc" {
				t.Errorf("expected_head_sha = %v", body["expected_head_sha"])
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"worktree_id":"01WT","worktree_dir":"/work/01WT"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewTCPClient(srv.URL, "")
	locator := GitHubPullRequestLocator{Owner: "acme", Repo: "api", Number: 7}
	inspection, err := c.GitHubPullRequestInspect(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.HeadOwner != "contributor" || inspection.HeadRepo != "api-fork" || inspection.HeadSHA != "abc" {
		t.Errorf("inspection = %+v", inspection)
	}
	launched, err := c.GitHubPullRequestLaunch(context.Background(), GitHubPullRequestLaunchRequest{
		GitHubPullRequestLocator: locator,
		ExpectedHeadSHA:          inspection.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if launched.WorktreeID != "01WT" || launched.WorktreeDir != "/work/01WT" {
		t.Errorf("launched = %+v", launched)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v", paths)
	}
}
