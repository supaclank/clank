package host_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	githubpkg "github.com/acksell/clank/internal/host/github"
)

// RemoteSyncStatus's PR half: the open PR's number/URL/base attach, and
// mergeability attaches only once GitHub has computed it — null stays
// omitted, and a failed detail fetch degrades to the deep-link fields.
// Fixture mirrors TestCreatePR_EndToEnd (real repo + bare origin +
// insteadOf redirect + stubbed API). Not parallel: Setenv + work-root
// globals.
func TestRemoteSyncStatus_PRMergeability(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required")
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv(githubpkg.ClientIDEnv, "Ov23li78UDBwea5WvI5v")

	const worktreeID = "01TESTWORKTREE0000000000"
	workdir := filepath.Join(homeDir, "work", worktreeID)
	bareDir := filepath.Join(homeDir, "remote.git")

	// Seed a repo whose feature branch is pushed (synced) so the status
	// call exercises the full fetch + PR-attach path.
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
	mustGit(t, workdir, "push", "origin", "feat-x:refs/heads/feat-x")

	store := githubpkg.NewStore(homeDir)
	if err := store.Write(githubpkg.Credentials{
		AccessToken: "gho_test",
		GitHubLogin: "axelengstrom",
		Scopes:      []string{"repo", "read:user"},
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	// The PR-detail response is swapped per case; the list response is
	// fixed (one open PR for the branch, base main).
	var mu sync.Mutex
	detailStatus := http.StatusOK
	detailBody := `{"number":7,"mergeable":false}`
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/acme/api/pulls" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"number":7,"html_url":"https://github.com/acme/api/pull/7",
				"head":{"sha":"abc"},"base":{"ref":"main"}}]`))
		case r.URL.Path == "/repos/acme/api/pulls/7" && r.Method == http.MethodGet:
			mu.Lock()
			status, body := detailStatus, detailBody
			mu.Unlock()
			if status != http.StatusOK {
				http.Error(w, "boom", status)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
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

	// Conflicting: mergeable=false lands on the wire.
	result, err := svc.RemoteSyncStatus(context.Background(), worktreeID)
	if err != nil {
		t.Fatalf("RemoteSyncStatus: %v", err)
	}
	if result.State != host.RemoteStateSynced {
		t.Errorf("State = %q, want synced", result.State)
	}
	if result.PRNumber != 7 || result.PRBaseBranch != "main" {
		t.Errorf("PR fields = #%d base %q, want #7 base main", result.PRNumber, result.PRBaseBranch)
	}
	if result.PRMergeable != githubpkg.MergeableStateConflicting {
		t.Errorf("PRMergeable = %q, want conflicting", result.PRMergeable)
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"pr_mergeable":"conflicting"`) ||
		!strings.Contains(string(wire), `"pr_base_branch":"main"`) {
		t.Errorf("wire JSON missing PR annotations: %s", wire)
	}

	// Still computing: mergeable=null stays omitted (absent ≠ clean).
	mu.Lock()
	detailBody = `{"number":7,"mergeable":null}`
	mu.Unlock()
	result, err = svc.RemoteSyncStatus(context.Background(), worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	if result.PRMergeable != "" {
		t.Errorf("PRMergeable = %q on null, want empty", result.PRMergeable)
	}
	if wire, _ := json.Marshal(result); strings.Contains(string(wire), "pr_mergeable") {
		t.Errorf("wire JSON carries pr_mergeable despite unknown: %s", wire)
	}

	// Detail fetch fails: deep-link fields survive, mergeability empty.
	mu.Lock()
	detailStatus = http.StatusInternalServerError
	mu.Unlock()
	result, err = svc.RemoteSyncStatus(context.Background(), worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	if result.PRNumber != 7 || result.PRURL == "" || result.PRBaseBranch != "main" {
		t.Errorf("PR fields lost on detail failure: %+v", result)
	}
	if result.PRMergeable != "" {
		t.Errorf("PRMergeable = %q after detail failure, want empty", result.PRMergeable)
	}
}
