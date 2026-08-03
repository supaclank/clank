package host_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/host"
	hoststore "github.com/supaclank/clank/internal/host/store"
)

func TestLaunchGitHubPullRequestReusesAndFastForwardsLocalCheckout(t *testing.T) {
	_, bare, featureSHA, mainSHA, apiURL := setupGitHubPullRequestLaunch(t)
	checkout := cloneGitHubPRLocalCheckout(t, bare, mainSHA)
	svc := newLocalRepoService(t, checkout)
	svc.GitHub().SetAPIBaseURL(apiURL)

	result, err := svc.LaunchGitHubPullRequest(context.Background(), githubPRLaunchRequest(featureSHA))
	if err != nil {
		t.Fatalf("LaunchGitHubPullRequest: %v", err)
	}
	resolvedCheckout, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if result.WorktreeID != "" || result.WorktreeDir != resolvedCheckout || result.Branch != "feature" {
		t.Errorf("result = %+v, want unmanaged checkout %s on feature", result, resolvedCheckout)
	}
	if got := strings.TrimSpace(runGitHubPRTestCommand(t, checkout, "git", "rev-parse", "HEAD")); got != featureSHA {
		t.Errorf("checkout HEAD = %s, want fast-forwarded %s", got, featureSHA)
	}
}

func TestLaunchGitHubPullRequestRefusesDirtyLocalCheckout(t *testing.T) {
	_, bare, featureSHA, mainSHA, apiURL := setupGitHubPullRequestLaunch(t)
	checkout := cloneGitHubPRLocalCheckout(t, bare, mainSHA)
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("local edits\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newLocalRepoService(t, checkout)
	svc.GitHub().SetAPIBaseURL(apiURL)

	_, err := svc.LaunchGitHubPullRequest(context.Background(), githubPRLaunchRequest(featureSHA))
	if !errors.Is(err, host.ErrWorktreeDirty) {
		t.Fatalf("error = %v, want ErrWorktreeDirty", err)
	}
	resolvedCheckout, resolveErr := filepath.EvalSymlinks(checkout)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if !strings.Contains(err.Error(), resolvedCheckout) {
		t.Errorf("error %q does not name checkout %q", err, resolvedCheckout)
	}
	if got := strings.TrimSpace(runGitHubPRTestCommand(t, checkout, "git", "rev-parse", "HEAD")); got != mainSHA {
		t.Errorf("dirty checkout HEAD = %s, want unchanged %s", got, mainSHA)
	}
}

func TestLaunchGitHubPullRequestRefusesLocalCommits(t *testing.T) {
	_, bare, featureSHA, _, apiURL := setupGitHubPullRequestLaunch(t)
	checkout := cloneGitHubPRLocalCheckout(t, bare, featureSHA)
	commitGitHubPRLocalEdit(t, checkout, "local commit")
	svc := newLocalRepoService(t, checkout)
	svc.GitHub().SetAPIBaseURL(apiURL)

	_, err := svc.LaunchGitHubPullRequest(context.Background(), githubPRLaunchRequest(featureSHA))
	if !errors.Is(err, host.ErrPullRequestLocalCommits) {
		t.Fatalf("error = %v, want ErrPullRequestLocalCommits", err)
	}
}

func TestLaunchGitHubPullRequestRefusesDivergedLocalBranch(t *testing.T) {
	_, bare, featureSHA, mainSHA, apiURL := setupGitHubPullRequestLaunch(t)
	checkout := cloneGitHubPRLocalCheckout(t, bare, mainSHA)
	commitGitHubPRLocalEdit(t, checkout, "diverged commit")
	svc := newLocalRepoService(t, checkout)
	svc.GitHub().SetAPIBaseURL(apiURL)

	_, err := svc.LaunchGitHubPullRequest(context.Background(), githubPRLaunchRequest(featureSHA))
	if !errors.Is(err, host.ErrRemoteDiverged) {
		t.Fatalf("error = %v, want ErrRemoteDiverged", err)
	}
}

func TestLaunchGitHubPullRequestRefusesAmbiguousLocalCheckouts(t *testing.T) {
	_, bare, featureSHA, _, apiURL := setupGitHubPullRequestLaunch(t)
	first := cloneGitHubPRLocalCheckout(t, bare, featureSHA)
	second := cloneGitHubPRLocalCheckout(t, bare, featureSHA)
	svc := newLocalRepoService(t, first, second)
	svc.GitHub().SetAPIBaseURL(apiURL)

	_, err := svc.LaunchGitHubPullRequest(context.Background(), githubPRLaunchRequest(featureSHA))
	if !errors.Is(err, host.ErrPullRequestRepoAmbiguous) {
		t.Fatalf("error = %v, want ErrPullRequestRepoAmbiguous", err)
	}
}

func TestLaunchGitHubPullRequestPrefersLocalCheckoutOverManagedCanonical(t *testing.T) {
	_, bare, featureSHA, _, apiURL := setupGitHubPullRequestLaunch(t)
	store, err := hoststore.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{agent.BackendOpenCode: &noopBackendManager{}},
		SessionsStore:   store,
		WorkRoot:        filepath.Join(t.TempDir(), "work"),
	})
	t.Cleanup(svc.Shutdown)
	svc.GitHub().SetAPIBaseURL(apiURL)

	managed, err := svc.LaunchGitHubPullRequest(context.Background(), githubPRLaunchRequest(featureSHA))
	if err != nil {
		t.Fatalf("initial managed launch: %v", err)
	}
	if managed.WorktreeID == "" {
		t.Fatal("initial launch did not create a managed worktree")
	}

	checkout := cloneGitHubPRLocalCheckout(t, bare, featureSHA)
	if err := store.UpsertSession(context.Background(), agent.SessionInfo{
		ID:        "01PRLOCALPREFERENCE00001",
		Backend:   agent.BackendOpenCode,
		Status:    agent.StatusIdle,
		GitRef:    agent.GitRef{LocalPath: checkout},
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	local, err := svc.LaunchGitHubPullRequest(context.Background(), githubPRLaunchRequest(featureSHA))
	if err != nil {
		t.Fatalf("launch with local checkout: %v", err)
	}
	resolvedCheckout, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if local.WorktreeID != "" || local.WorktreeDir != resolvedCheckout {
		t.Errorf("result = %+v, want local checkout %s", local, resolvedCheckout)
	}
}

func cloneGitHubPRLocalCheckout(t *testing.T, bare, startSHA string) string {
	t.Helper()
	checkout := filepath.Join(t.TempDir(), "api")
	runGitHubPRTestCommand(t, t.TempDir(), "git", "clone", bare, checkout)
	runGitHubPRTestCommand(t, checkout, "git", "remote", "set-url", "origin", "https://github.com/acme/api.git")
	runGitHubPRTestCommand(t, checkout, "git", "checkout", "-B", "feature", startSHA)
	return checkout
}

func commitGitHubPRLocalEdit(t *testing.T, checkout, message string) {
	t.Helper()
	runGitHubPRTestCommand(t, checkout, "git", "config", "user.email", "test@example.com")
	runGitHubPRTestCommand(t, checkout, "git", "config", "user.name", "Test")
	runGitHubPRTestCommand(t, checkout, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(checkout, "local.txt"), []byte(message+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHubPRTestCommand(t, checkout, "git", "add", "local.txt")
	runGitHubPRTestCommand(t, checkout, "git", "commit", "-m", message)
}

func githubPRLaunchRequest(headSHA string) host.GitHubPullRequestLaunchRequest {
	return host.GitHubPullRequestLaunchRequest{
		GitHubPullRequestLocator: host.GitHubPullRequestLocator{Owner: "acme", Repo: "api", Number: 7},
		ExpectedHeadSHA:          headSHA,
	}
}
