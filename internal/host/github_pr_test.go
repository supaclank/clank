package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	// origin with a github.com-looking URL (so ParseGitHubRemote
	// finds owner/repo). Use url.<bare>.insteadOf to redirect any
	// git operation against that github.com URL to the local bare
	// repo, which lets fetch/push exercise the production code path
	// without needing real network.
	mustGit(t, "", "init", "--bare", bareDir)
	mustGit(t, workdir, "remote", "add", "origin", "https://github.com/acme/api.git")
	mustGit(t, workdir, "config", "url."+bareDir+".insteadOf", "https://github.com/acme/api.git")
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
			body, _ := io.ReadAll(r.Body)
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

// TestCreatePR_CommitsUncommittedWork is the regression for "Create
// pull request" failing with ErrNothingToPush while the worktree had
// real (but uncommitted) work: the branch ref sat at base, so the
// commits-ahead check saw zero. Users had to tap "Push to remote"
// (which auto-commits) before the PR button worked. CreatePR must
// auto-commit dirty work itself, exactly like PushToRemote.
func TestCreatePR_CommitsUncommittedWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000300"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	bareDir := filepath.Join(homeDir, "remote.git")

	// Feature branch at the SAME commit as main — all work uncommitted.
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
	baseSHA := strings.TrimSpace(mustGit(t, workdir, "rev-parse", "HEAD"))
	// Uncommitted work: one modified tracked file + one untracked file.
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "NEW"), []byte("agent-made file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mustGit(t, "", "init", "--bare", bareDir)
	mustGit(t, workdir, "remote", "add", "origin", "https://github.com/acme/api.git")
	mustGit(t, workdir, "config", "url."+bareDir+".insteadOf", "https://github.com/acme/api.git")
	mustGit(t, workdir, "push", "origin", "main:refs/heads/main")

	store := githubpkg.NewStore(homeDir)
	if err := store.Write(githubpkg.Credentials{
		AccessToken: "gho_test",
		GitHubLogin: "axelengstrom",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"number": 7,
				"html_url": "https://github.com/acme/api/pull/7",
				"head": {"sha": "ignored"}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
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

	result, err := svc.CreatePR(context.Background(), worktreeID, host.CreatePRRequest{
		Title: "feat: v2",
		Body:  "uncommitted work should ride along",
		Base:  "main",
	})
	if err != nil {
		t.Fatalf("CreatePR with uncommitted work: %v", err)
	}
	if !result.Committed {
		t.Error("Committed = false, want true (dirty tree was auto-committed)")
	}
	if result.HeadSHA == baseSHA {
		t.Errorf("HeadSHA still at base %s — dirty work wasn't committed", baseSHA)
	}

	// The auto-commit (with both files) reached the bare remote.
	if out := mustGit(t, bareDir, "show-ref", "refs/heads/feat-x"); !strings.Contains(out, result.HeadSHA) {
		t.Errorf("bare feat-x ref = %q, want HeadSHA %s", out, result.HeadSHA)
	}
	files := mustGit(t, bareDir, "ls-tree", "--name-only", "refs/heads/feat-x")
	if !strings.Contains(files, "NEW") {
		t.Errorf("pushed tree missing untracked-then-committed file NEW:\n%s", files)
	}
	msg := mustGit(t, bareDir, "log", "-1", "--format=%s", "refs/heads/feat-x")
	if !strings.Contains(msg, "clank: update feat-x") {
		t.Errorf("auto-commit message = %q, want the clank push template", msg)
	}
}

// TestCreatePR_CleanAtBaseStillNothingToPush pins that the auto-commit
// does not soften the genuine no-work guard: clean tree, branch at
// base → still ErrNothingToPush.
func TestCreatePR_CleanAtBaseStillNothingToPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000301"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	bareDir := filepath.Join(homeDir, "remote.git")

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

	mustGit(t, "", "init", "--bare", bareDir)
	mustGit(t, workdir, "remote", "add", "origin", "https://github.com/acme/api.git")
	mustGit(t, workdir, "config", "url."+bareDir+".insteadOf", "https://github.com/acme/api.git")
	mustGit(t, workdir, "push", "origin", "main:refs/heads/main")

	store := githubpkg.NewStore(homeDir)
	if err := store.Write(githubpkg.Credentials{
		AccessToken: "gho_test",
		GitHubLogin: "axelengstrom",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

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
	if !errors.Is(err, host.ErrNothingToPush) {
		t.Fatalf("err = %v, want ErrNothingToPush", err)
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

// TestCreatePR_RefusesPushToUnrelatedRepo is the wrong-repo safety
// regression. Two bare repos with completely separate histories
// (independent `git init` lineages so they share no commit SHAs)
// stand in for "correct destination" and "wrong destination." We
// point origin at the wrong one and confirm CreatePR refuses with
// ErrNoCommonAncestor without leaking any refs.
func TestCreatePR_RefusesPushToUnrelatedRepo(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000099"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	wrongBareDir := filepath.Join(homeDir, "wrong.git")

	// Seed the user's worktree on its own history.
	mustGit(t, "", "init", workdir)
	mustGit(t, workdir, "config", "user.email", "test@example.com")
	mustGit(t, workdir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("our v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "our base")
	mustGit(t, workdir, "branch", "-M", "main")
	mustGit(t, workdir, "checkout", "-b", "feat-x")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("our v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "our work")

	// Seed the WRONG bare repo on a completely independent history
	// — separate `git init`, separate commits, different SHAs.
	wrongStaging := filepath.Join(homeDir, "wrong-staging")
	mustGit(t, "", "init", wrongStaging)
	mustGit(t, wrongStaging, "config", "user.email", "other@example.com")
	mustGit(t, wrongStaging, "config", "user.name", "other")
	if err := os.WriteFile(filepath.Join(wrongStaging, "DIFFERENT"), []byte("completely different content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wrongStaging, "add", "DIFFERENT")
	mustGit(t, wrongStaging, "commit", "-m", "their unrelated base")
	mustGit(t, wrongStaging, "branch", "-M", "main")
	mustGit(t, "", "init", "--bare", wrongBareDir)
	mustGit(t, wrongStaging, "remote", "add", "wrongorigin", wrongBareDir)
	mustGit(t, wrongStaging, "push", "wrongorigin", "main")

	// Configure the user's worktree to point origin at the WRONG repo.
	mustGit(t, workdir, "remote", "add", "origin", "https://github.com/wrong/repo.git")
	mustGit(t, workdir, "config", "url."+wrongBareDir+".insteadOf", "https://github.com/wrong/repo.git")

	// Seed credentials.
	store := githubpkg.NewStore(homeDir)
	if err := store.Write(githubpkg.Credentials{
		AccessToken: "gho_test",
		GitHubLogin: "axelengstrom",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	// Snapshot the wrong bare repo's refs BEFORE so we can assert
	// nothing changed.
	refsBefore := mustGit(t, wrongBareDir, "show-ref")

	prev := host.SetWorkRootForTest(filepath.Join(homeDir, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	_, err := svc.CreatePR(context.Background(), worktreeID, host.CreatePRRequest{
		Title: "feat: leak attempt",
		Body:  "this should not land",
		Base:  "main",
	})
	if !errors.Is(err, host.ErrNoCommonAncestor) {
		t.Fatalf("err = %v, want ErrNoCommonAncestor", err)
	}

	// The critical safety assertion: the wrong bare repo's refs are
	// completely unchanged. No leak.
	refsAfter := mustGit(t, wrongBareDir, "show-ref")
	if refsBefore != refsAfter {
		t.Errorf("wrong bare repo's refs changed after refused push!\nbefore:\n%s\nafter:\n%s",
			refsBefore, refsAfter)
	}
	if strings.Contains(refsAfter, "feat-x") {
		t.Errorf("wrong bare repo leaked the feat-x ref:\n%s", refsAfter)
	}
}

// TestPreviewPR_GitHubOrigin pins the happy-path preview: origin
// parses to a github.com URL, sheet would render the Open PR form
// with the destination callout populated.
func TestPreviewPR_GitHubOrigin(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000200"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	mustGit(t, "", "init", workdir)
	mustGit(t, workdir, "config", "user.email", "test@example.com")
	mustGit(t, workdir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "init")
	mustGit(t, workdir, "branch", "-M", "main")
	mustGit(t, workdir, "checkout", "-b", "feat-x")
	mustGit(t, workdir, "remote", "add", "origin", "https://github.com/acme/api.git")

	prev := host.SetWorkRootForTest(filepath.Join(homeDir, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	result, err := svc.PreviewPR(context.Background(), worktreeID)
	if err != nil {
		t.Fatalf("PreviewPR: %v", err)
	}
	if result.OriginState != host.PreviewOriginGitHub {
		t.Errorf("OriginState = %q, want github", result.OriginState)
	}
	if result.Owner != "acme" || result.Repo != "api" {
		t.Errorf("Owner/Repo = %q/%q, want acme/api", result.Owner, result.Repo)
	}
	if result.HeadBranch != "feat-x" {
		t.Errorf("HeadBranch = %q, want feat-x", result.HeadBranch)
	}
	if len(result.HeadSHA) != 40 {
		t.Errorf("HeadSHA = %q, want 40 chars", result.HeadSHA)
	}
}

// TestPreviewPR_NoOrigin pins the no-origin classification — the
// sheet should render the no-origin banner and hide the form.
func TestPreviewPR_NoOrigin(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000201"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	mustGit(t, "", "init", workdir)
	mustGit(t, workdir, "config", "user.email", "test@example.com")
	mustGit(t, workdir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "init")
	mustGit(t, workdir, "branch", "-M", "main")
	mustGit(t, workdir, "checkout", "-b", "feat-x")
	// Deliberately no `git remote add origin`.

	prev := host.SetWorkRootForTest(filepath.Join(homeDir, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	result, err := svc.PreviewPR(context.Background(), worktreeID)
	if err != nil {
		t.Fatalf("PreviewPR: %v", err)
	}
	if result.OriginState != host.PreviewOriginNone {
		t.Errorf("OriginState = %q, want no_origin", result.OriginState)
	}
	if result.Owner != "" || result.Repo != "" {
		t.Errorf("Owner/Repo should be empty, got %q/%q", result.Owner, result.Repo)
	}
	// Head metadata is still useful even without origin.
	if result.HeadBranch != "feat-x" {
		t.Errorf("HeadBranch = %q, want feat-x", result.HeadBranch)
	}
}

// TestPreviewPR_NonGitHubOrigin pins the non-github classification —
// the sheet should render the non-github banner with the actual
// host name (gitlab.com) so the user knows what their origin
// points to.
func TestPreviewPR_NonGitHubOrigin(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000202"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	mustGit(t, "", "init", workdir)
	mustGit(t, workdir, "config", "user.email", "test@example.com")
	mustGit(t, workdir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(workdir, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workdir, "add", "README")
	mustGit(t, workdir, "commit", "-m", "init")
	mustGit(t, workdir, "branch", "-M", "main")
	mustGit(t, workdir, "checkout", "-b", "feat-x")
	mustGit(t, workdir, "remote", "add", "origin", "https://gitlab.com/acme/api.git")

	prev := host.SetWorkRootForTest(filepath.Join(homeDir, "work"))
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
	})
	t.Cleanup(svc.Shutdown)

	result, err := svc.PreviewPR(context.Background(), worktreeID)
	if err != nil {
		t.Fatalf("PreviewPR: %v", err)
	}
	if result.OriginState != host.PreviewOriginNonGitHub {
		t.Errorf("OriginState = %q, want non_github", result.OriginState)
	}
	if result.NonGitHubHost != "gitlab.com" {
		t.Errorf("NonGitHubHost = %q, want gitlab.com", result.NonGitHubHost)
	}
	if result.Owner != "" || result.Repo != "" {
		t.Errorf("Owner/Repo should be empty, got %q/%q", result.Owner, result.Repo)
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
