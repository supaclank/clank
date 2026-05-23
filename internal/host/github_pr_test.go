package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// TestCreatePR_EndToEnd is the host's integration test for the
// PR-creation feature. Real git repo + bare remote + fake GitHub API.
// Covers:
//   - Worktree resolution by ID.
//   - Credential read (manager owns the store).
//   - Branch / remote / HEAD inspection via internal/git.
//   - Push against a bare repo, asserting the ref landed.
//   - PR API call against an httptest.Server, asserting body shape.
//   - Regression: no token substring leaks to .git/config after push.
func TestCreatePR_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000000"
	const sentinel = "gho_secret_token_value"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	bareDir := filepath.Join(homeDir, "remote.git")

	// 1) Seed a real git repo with a base branch and a feature
	// branch with a commit ahead.
	mustGit(t, "", "init", workdir)
	mustGit(t, workdir, "config", "user.email", "test@example.com")
	mustGit(t, workdir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "base")
	mustGit(t, workdir, "branch", "-M", "main")
	// Feature branch + commit ahead of main.
	mustGit(t, workdir, "checkout", "-b", "feat-x")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "feature work")

	// 2) Stand up a bare repo as the "remote", and configure
	// origin with a github.com-looking fetch URL (so the parser
	// finds owner/repo) + a file:// push URL so the actual push
	// works without network.
	mustGit(t, "", "init", "--bare", bareDir)
	mustGit(t, workdir, "remote", "add", "origin", "https://github.com/acme/api.git")
	mustGit(t, workdir, "remote", "set-url", "--push", "origin", bareDir)
	// Push main first so the bare has the base branch.
	mustGit(t, workdir, "push", "origin", "main:refs/heads/main")

	// 3) Drop a credential the Manager will find. (Mirrors what the
	// device flow would do; we skip the OAuth dance for this test.)
	store := githubpkg.NewStore(homeDir)
	if err := store.Write(githubpkg.Credentials{
		AccessToken: sentinel,
		GitHubLogin: "axelengstrom",
		Scopes:      []string{"repo", "read:user"},
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	// 4) Fake GitHub API: tracks the POST body so we can assert
	// what the host sent.
	var (
		gotPath  atomic.Value // string
		gotAuth  atomic.Value // string
		gotBody  atomic.Value // []byte
		postReqs atomic.Int64
	)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls") {
			postReqs.Add(1)
			gotPath.Store(r.URL.Path)
			gotAuth.Store(r.Header.Get("Authorization"))
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			gotBody.Store(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"number": 99,
				"html_url": "https://github.com/acme/api/pull/99",
				"head": {"sha": "ignored"}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(apiSrv.Close)

	// 5) Build host.Service and point its github.Manager at the fake.
	prev := host.SetWorkRootForTest(filepath.Join(homeDir, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)
	svc.GitHub().SetAPIBaseURL(apiSrv.URL)

	// 6) Call CreatePR.
	result, err := svc.CreatePR(context.Background(), worktreeID, host.CreatePRRequest{
		Title: "feat: add v2",
		Body:  "bumps README to v2",
		Base:  "main",
	})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}

	// 7) Assert wire-level result.
	if result.PRNumber != 99 {
		t.Errorf("PRNumber = %d, want 99", result.PRNumber)
	}
	if result.PRURL != "https://github.com/acme/api/pull/99" {
		t.Errorf("PRURL = %q", result.PRURL)
	}
	if result.HeadBranch != "feat-x" {
		t.Errorf("HeadBranch = %q, want feat-x", result.HeadBranch)
	}
	if result.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", result.BaseBranch)
	}
	if len(result.HeadSHA) != 40 {
		t.Errorf("HeadSHA = %q, want 40-char hex", result.HeadSHA)
	}

	// 8) The PR API call landed at the right path with the right
	// Bearer token.
	if got := gotPath.Load(); got != "/repos/acme/api/pulls" {
		t.Errorf("API path = %v, want /repos/acme/api/pulls", got)
	}
	if got := gotAuth.Load(); got != "Bearer "+sentinel {
		t.Errorf("API Authorization = %v", got)
	}
	if postReqs.Load() != 1 {
		t.Errorf("postReqs = %d, want 1", postReqs.Load())
	}
	bodyBytes, _ := gotBody.Load().([]byte)
	var prBody githubpkg.CreatePRInput
	if err := json.Unmarshal(bodyBytes, &prBody); err != nil {
		t.Fatalf("decode PR body: %v body=%s", err, bodyBytes)
	}
	if prBody.Title != "feat: add v2" || prBody.Body != "bumps README to v2" ||
		prBody.Head != "feat-x" || prBody.Base != "main" {
		t.Errorf("PR body wrong: %+v", prBody)
	}

	// 9) The push reached the bare repo.
	out := mustGit(t, bareDir, "show-ref", "refs/heads/feat-x")
	if !strings.Contains(out, "refs/heads/feat-x") {
		t.Errorf("bare repo missing pushed feat-x ref:\n%s", out)
	}

	// 10) Regression: the token never lands in .git/config.
	cfg, err := os.ReadFile(filepath.Join(workdir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), sentinel) {
		t.Errorf(".git/config leaked the token!\n%s", cfg)
	}
	if strings.Contains(string(cfg), "extraheader") {
		t.Errorf(".git/config contains 'extraheader' — expected only process-arg use:\n%s", cfg)
	}
}

func TestCreatePR_NotConnected(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000001"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	mustGit(t, "", "init", workdir)
	mustGit(t, workdir, "config", "user.email", "test@example.com")
	mustGit(t, workdir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "hi")
	mustGit(t, workdir, "branch", "-M", "main")
	mustGit(t, workdir, "checkout", "-b", "feat")

	prev := host.SetWorkRootForTest(filepath.Join(homeDir, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	_, err := svc.CreatePR(context.Background(), worktreeID, host.CreatePRRequest{
		Title: "x", Body: "y", Base: "main",
	})
	if !errors.Is(err, host.ErrGitHubNotConnected) {
		t.Fatalf("err = %v, want ErrGitHubNotConnected", err)
	}
}

func TestCreatePR_MissingFields(t *testing.T) {
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	_, err := svc.CreatePR(context.Background(), "any-id", host.CreatePRRequest{Title: ""})
	if !errors.Is(err, host.ErrPRMissingField) {
		t.Errorf("missing title: err = %v, want ErrPRMissingField", err)
	}
	_, err = svc.CreatePR(context.Background(), "any-id", host.CreatePRRequest{Title: "x", Base: ""})
	if !errors.Is(err, host.ErrPRMissingField) {
		t.Errorf("missing base: err = %v, want ErrPRMissingField", err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %q: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}
