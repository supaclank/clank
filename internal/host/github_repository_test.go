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
	"sync/atomic"
	"testing"

	"github.com/acksell/clank/internal/git"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

func TestLaunchGitHubRepositoryForksFreshWorktreeFromDefaultBranch(t *testing.T) {
	// Not parallel: the GitHub clone base override is package-global.
	t.Setenv("HOME", t.TempDir())

	source := t.TempDir()
	runGitHubRepositoryTestCommand(t, source, "git", "init", "-b", "trunk")
	runGitHubRepositoryTestCommand(t, source, "git", "config", "user.email", "test@example.com")
	runGitHubRepositoryTestCommand(t, source, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHubRepositoryTestCommand(t, source, "git", "add", ".")
	runGitHubRepositoryTestCommand(t, source, "git", "commit", "-m", "initial")
	wantSHA := strings.TrimSpace(runGitHubRepositoryTestCommand(t, source, "git", "rev-parse", "HEAD"))

	repositories := t.TempDir()
	bare := filepath.Join(repositories, "acme", "api.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitHubRepositoryTestCommand(t, repositories, "git", "clone", "--bare", source, bare)
	runGitHubRepositoryTestCommand(t, bare, "git", "config", "uploadpack.allowFilter", "true")
	previousCloneBase := host.SetGitHubCloneBaseForTest("file://" + repositories)
	t.Cleanup(func() { host.SetGitHubCloneBaseForTest(previousCloneBase) })

	var defaultBranch atomic.Value
	defaultBranch.Store("trunk")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/api" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want anonymous public-repo inspection", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"name":"api","full_name":"acme/api","html_url":"https://github.com/acme/api",
			"description":"Test API","private":false,"default_branch":%q,
			"owner":{"login":"acme"}
		}`, defaultBranch.Load())
	}))
	t.Cleanup(api.Close)

	workRoot := filepath.Join(t.TempDir(), "work")
	previousWorkRoot := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(previousWorkRoot) })
	svc := newTestService(t)
	svc.GitHub().SetAPIBaseURL(api.URL)

	inspection, err := svc.InspectGitHubRepository(context.Background(), host.GitHubRepositoryLocator{Owner: "acme", Repo: "api"})
	if err != nil {
		t.Fatalf("InspectGitHubRepository: %v", err)
	}
	if inspection.DefaultBranch != "trunk" || inspection.Description != "Test API" || inspection.IsPrivate {
		t.Fatalf("inspection = %+v", inspection)
	}

	first, err := svc.LaunchGitHubRepository(context.Background(), host.GitHubRepositoryLocator{Owner: "acme", Repo: "api"})
	if err != nil {
		t.Fatalf("LaunchGitHubRepository: %v", err)
	}
	if first.Branch == "" || first.Branch == "trunk" {
		t.Errorf("editing branch = %q, want a fresh branch", first.Branch)
	}
	if first.DefaultBranch != "trunk" || first.RepoSlug != "acme__api" || first.WorktreeID == "" {
		t.Errorf("launch = %+v", first)
	}
	if got, err := git.HeadCommit(first.WorktreeDir); err != nil || got != wantSHA {
		t.Errorf("editing worktree HEAD = %q, %v; want %s", got, err, wantSHA)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("new remote tip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHubRepositoryTestCommand(t, source, "git", "commit", "-am", "advance default")
	newRemoteSHA := strings.TrimSpace(runGitHubRepositoryTestCommand(t, source, "git", "rev-parse", "HEAD"))
	runGitHubRepositoryTestCommand(t, source, "git", "push", "file://"+bare, "trunk:trunk")

	second, err := svc.LaunchGitHubRepository(context.Background(), host.GitHubRepositoryLocator{Owner: "acme", Repo: "api"})
	if err != nil {
		t.Fatalf("second LaunchGitHubRepository: %v", err)
	}
	if second.WorktreeID == first.WorktreeID || second.Branch == first.Branch {
		t.Errorf("second launch reused editing workspace: first=%+v second=%+v", first, second)
	}
	if got, err := git.HeadCommit(second.WorktreeDir); err != nil || got != newRemoteSHA {
		t.Errorf("second editing worktree HEAD = %q, %v; want refreshed remote %s", got, err, newRemoteSHA)
	}

	runGitHubRepositoryTestCommand(t, source, "git", "checkout", "-b", "next")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("new default branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHubRepositoryTestCommand(t, source, "git", "commit", "-am", "change default")
	newDefaultSHA := strings.TrimSpace(runGitHubRepositoryTestCommand(t, source, "git", "rev-parse", "HEAD"))
	runGitHubRepositoryTestCommand(t, source, "git", "push", "file://"+bare, "next:next")
	runGitHubRepositoryTestCommand(t, bare, "git", "symbolic-ref", "HEAD", "refs/heads/next")
	defaultBranch.Store("next")

	third, err := svc.LaunchGitHubRepository(context.Background(), host.GitHubRepositoryLocator{Owner: "acme", Repo: "api"})
	if err != nil {
		t.Fatalf("launch after default branch changed: %v", err)
	}
	if third.DefaultBranch != "next" {
		t.Errorf("third default branch = %q, want next", third.DefaultBranch)
	}
	if got, err := git.HeadCommit(third.WorktreeDir); err != nil || got != newDefaultSHA {
		t.Errorf("third editing worktree HEAD = %q, %v; want new default %s", got, err, newDefaultSHA)
	}
}

func runGitHubRepositoryTestCommand(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestInspectGitHubRepositoryRetriesConnectedCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var anonymousCalls, authenticatedCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			anonymousCalls++
			http.NotFound(w, r)
			return
		}
		authenticatedCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"name":"secret","html_url":"https://github.com/acme/secret","private":true,"default_branch":"main","owner":{"login":"acme"}}`)
	}))
	t.Cleanup(api.Close)
	svc := newTestService(t)
	svc.GitHub().SetAPIBaseURL(api.URL)
	if err := svc.GitHub().Store().Write(githubpkg.Credentials{AccessToken: "gho_private"}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.InspectGitHubRepository(context.Background(), host.GitHubRepositoryLocator{Owner: "acme", Repo: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsPrivate || anonymousCalls != 1 || authenticatedCalls != 1 {
		t.Errorf("inspection=%+v calls anonymous=%d authenticated=%d", got, anonymousCalls, authenticatedCalls)
	}
}

func TestInspectGitHubRepositoryRequiresConnectionForPrivateRepository(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	api := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(api.Close)
	svc := newTestService(t)
	svc.GitHub().SetAPIBaseURL(api.URL)

	_, err := svc.InspectGitHubRepository(context.Background(), host.GitHubRepositoryLocator{Owner: "acme", Repo: "secret"})
	if !errors.Is(err, host.ErrGitHubRepositoryConnectionRequired) {
		t.Fatalf("error = %v, want ErrGitHubRepositoryConnectionRequired", err)
	}
}
