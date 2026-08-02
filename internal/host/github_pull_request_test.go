package host_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

func TestInspectGitHubPullRequestRequiresConnectionForAnonymousNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	api := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(api.Close)
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{agent.BackendOpenCode: &noopBackendManager{}},
		WorkRoot:        filepath.Join(t.TempDir(), "work"),
	})
	t.Cleanup(svc.Shutdown)
	svc.GitHub().SetAPIBaseURL(api.URL)
	locator := host.GitHubPullRequestLocator{Owner: "acme", Repo: "private", Number: 7}

	_, err := svc.InspectGitHubPullRequest(context.Background(), locator)
	if !errors.Is(err, host.ErrGitHubConnectionRequired) {
		t.Fatalf("anonymous error = %v, want ErrGitHubConnectionRequired", err)
	}
	if err := svc.GitHub().Store().Write(githubpkg.Credentials{AccessToken: "gho_test"}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.InspectGitHubPullRequest(context.Background(), locator)
	if !errors.Is(err, host.ErrNotFound) {
		t.Fatalf("authenticated error = %v, want ErrNotFound", err)
	}
}

func TestLaunchGitHubPullRequestChecksOutApprovedPublicRevision(t *testing.T) {
	svc, _, featureSHA, _ := setupGitHubPullRequestLaunch(t)
	ctx := context.Background()
	req := host.GitHubPullRequestLaunchRequest{
		GitHubPullRequestLocator: host.GitHubPullRequestLocator{Owner: "acme", Repo: "api", Number: 7},
		ExpectedHeadSHA:          featureSHA,
	}

	first, err := svc.LaunchGitHubPullRequest(ctx, req)
	if err != nil {
		t.Fatalf("LaunchGitHubPullRequest: %v", err)
	}
	if first.WorktreeID == "" || first.DisplayName != "api#7" || first.OriginRepo != "acme/api" {
		t.Errorf("result = %+v", first)
	}
	if !strings.HasPrefix(first.Branch, "clank/pr-7-") {
		t.Errorf("branch = %q", first.Branch)
	}
	gotSHA, err := git.HeadCommit(first.WorktreeDir)
	if err != nil {
		t.Fatal(err)
	}
	if gotSHA != featureSHA {
		t.Errorf("worktree HEAD = %s, want approved %s", gotSHA, featureSHA)
	}

	again, err := svc.LaunchGitHubPullRequest(ctx, req)
	if err != nil {
		t.Fatalf("idempotent launch: %v", err)
	}
	if again.WorktreeID != first.WorktreeID {
		t.Errorf("second launch worktree = %s, want %s", again.WorktreeID, first.WorktreeID)
	}

	canonical, err := git.CommonDir(first.WorktreeDir)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := git.GetLocalConfig(canonical, "credential.helper")
	if err != nil {
		t.Fatal(err)
	}
	if helper != "" {
		t.Errorf("public launch credential.helper = %q, want anonymous canonical", helper)
	}
}

func TestLaunchGitHubPullRequestRejectsRefThatMovedAfterApproval(t *testing.T) {
	svc, bare, featureSHA, mainSHA := setupGitHubPullRequestLaunch(t)
	runGitHubPRTestCommand(t, bare, "git", "update-ref", "refs/pull/7/head", mainSHA)

	_, err := svc.LaunchGitHubPullRequest(context.Background(), host.GitHubPullRequestLaunchRequest{
		GitHubPullRequestLocator: host.GitHubPullRequestLocator{Owner: "acme", Repo: "api", Number: 7},
		ExpectedHeadSHA:          featureSHA,
	})
	if !errors.Is(err, host.ErrPullRequestChanged) {
		t.Fatalf("error = %v, want ErrPullRequestChanged", err)
	}
}

func setupGitHubPullRequestLaunch(t *testing.T) (*host.Service, string, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	runGitHubPRTestCommand(t, src, "git", "init", "-b", "main")
	runGitHubPRTestCommand(t, src, "git", "config", "user.email", "test@example.com")
	runGitHubPRTestCommand(t, src, "git", "config", "user.name", "Test")
	runGitHubPRTestCommand(t, src, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHubPRTestCommand(t, src, "git", "add", ".")
	runGitHubPRTestCommand(t, src, "git", "commit", "-m", "main")
	mainSHA := strings.TrimSpace(runGitHubPRTestCommand(t, src, "git", "rev-parse", "HEAD"))
	runGitHubPRTestCommand(t, src, "git", "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHubPRTestCommand(t, src, "git", "commit", "-am", "feature")
	featureSHA := strings.TrimSpace(runGitHubPRTestCommand(t, src, "git", "rev-parse", "HEAD"))

	reposRoot := t.TempDir()
	bare := filepath.Join(reposRoot, "acme", "api.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitHubPRTestCommand(t, reposRoot, "git", "clone", "--bare", src, bare)
	runGitHubPRTestCommand(t, bare, "git", "config", "uploadpack.allowFilter", "true")
	runGitHubPRTestCommand(t, bare, "git", "update-ref", "refs/pull/7/head", featureSHA)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/api/pulls/7" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"number":7,"title":"Feature","html_url":"https://github.com/acme/api/pull/7",
			"head":{"ref":"feature","sha":%q},"base":{"ref":"main","repo":{"private":false}},
			"user":{"login":"octocat"}
		}`, featureSHA)
	}))
	t.Cleanup(api.Close)

	previousCloneBase := host.SetGitHubCloneBaseForTest("file://" + reposRoot)
	t.Cleanup(func() { host.SetGitHubCloneBaseForTest(previousCloneBase) })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{agent.BackendOpenCode: &noopBackendManager{}},
		WorkRoot:        filepath.Join(t.TempDir(), "work"),
	})
	t.Cleanup(svc.Shutdown)
	svc.GitHub().SetAPIBaseURL(api.URL)
	return svc, bare, featureSHA, mainSHA
}

func runGitHubPRTestCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
