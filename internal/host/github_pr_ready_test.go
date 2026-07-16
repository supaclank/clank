package host_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// markReadyFixture seeds a worktree on a feature branch with a GitHub
// origin plus a connected credential, and returns the service. No
// bare remote — MarkPRReady never touches the git network, only the
// (stubbed) GitHub API. Not parallel: Setenv + work-root globals.
func markReadyFixture(t *testing.T, worktreeID string, api http.Handler) *host.Service {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	workdir := filepath.Join(homeDir, "work", worktreeID)
	mustGit(t, "", "init", workdir)
	mustGit(t, workdir, "config", "user.email", "test@example.com")
	mustGit(t, workdir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "base")
	mustGit(t, workdir, "branch", "-M", "main")
	mustGit(t, workdir, "checkout", "-b", "feat-x")
	mustGit(t, workdir, "remote", "add", "origin", "https://github.com/acme/api.git")

	store := githubpkg.NewStore(homeDir)
	if err := store.Write(githubpkg.Credentials{
		AccessToken: "gho_test",
		GitHubLogin: "axelengstrom",
		Scopes:      []string{"repo", "read:user"},
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)

	prev := host.SetWorkRootForTest(filepath.Join(homeDir, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)
	svc.GitHub().SetAPIBaseURL(apiSrv.URL)
	return svc
}

// The happy path: branch has an open draft PR → the GraphQL mutation
// fires and the PR's number/URL come back for the client toast.
func TestMarkPRReady_EndToEnd(t *testing.T) {
	var gqlReqs atomic.Int64
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/pulls":
			_, _ = w.Write([]byte(`[{"number":7,"html_url":"https://github.com/acme/api/pull/7",
				"draft":true,"head":{"sha":"abc"},"base":{"ref":"main"}}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"draft":true,"node_id":"PR_kwDOtest7"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			gqlReqs.Add(1)
			_, _ = w.Write([]byte(`{"data":{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":false}}}}`))
		default:
			http.NotFound(w, r)
		}
	})
	svc := markReadyFixture(t, "01TESTWORKTREE0000000042", api)

	result, err := svc.MarkPRReady(context.Background(), "01TESTWORKTREE0000000042")
	if err != nil {
		t.Fatalf("MarkPRReady: %v", err)
	}
	if result.PRNumber != 7 {
		t.Errorf("PRNumber = %d, want 7", result.PRNumber)
	}
	if result.PRURL != "https://github.com/acme/api/pull/7" {
		t.Errorf("PRURL = %q", result.PRURL)
	}
	if gqlReqs.Load() != 1 {
		t.Errorf("graphql requests = %d, want 1", gqlReqs.Load())
	}
}

// No open PR for the branch is a distinct, client-actionable failure —
// the UI refreshes its remote status instead of showing a generic error.
func TestMarkPRReady_NoOpenPR(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/pulls" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	})
	svc := markReadyFixture(t, "01TESTWORKTREE0000000043", api)

	_, err := svc.MarkPRReady(context.Background(), "01TESTWORKTREE0000000043")
	if !errors.Is(err, host.ErrNoOpenPRForBranch) {
		t.Fatalf("err = %v, want ErrNoOpenPRForBranch", err)
	}
}
