package host_test

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	clanksync "github.com/acksell/clank/internal/sync"
)

// TestWorkDirFor_UsesGitSyncSource proves that when a host is
// configured with GitSyncSource, workDirFor clones from the sync
// source's smart-HTTP endpoint (not from ref.RemoteURL) and validates
// the bearer token. This is the contract sandboxes rely on to fetch
// repos that aren't reachable from inside the sandbox network.
func TestWorkDirFor_UsesGitSyncSource(t *testing.T) {
	const fakeURL = "https://example.com/private/repo.git"
	const expectedToken = "tkn-abc"

	// 1. A bare git repo on disk with one commit on "main", which
	// stands in for the cloud hub's mirror.
	srcRepo := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main", srcRepo)
	if err := os.WriteFile(filepath.Join(srcRepo, "synced.txt"), []byte("via sync source\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit("-C", srcRepo, "add", "synced.txt")
	runGit("-C", srcRepo, "commit", "-m", "initial")
	bareRoot := t.TempDir()
	bare := filepath.Join(bareRoot, "repo.git")
	runGit("clone", "--bare", srcRepo, bare)

	// 2. Test server emulating the cloud hub's bearer-protected smart-HTTP path.
	gitExecPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	gitHTTPBackend := filepath.Join(strings.TrimSpace(string(gitExecPath)), "git-http-backend")
	repoKey := clanksync.RepoKey(fakeURL)
	prefix := "/sync/repos/" + repoKey + "/git"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expectedToken {
			http.Error(w, "unauthorized", 401)
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		cgiURL := *r.URL
		cgiURL.Path = "/repo.git" + rest
		req := r.Clone(r.Context())
		req.URL = &cgiURL
		(&cgi.Handler{
			Path: gitHTTPBackend,
			Dir:  bareRoot,
			Env: []string{
				"GIT_PROJECT_ROOT=" + bareRoot,
				"GIT_HTTP_EXPORT_ALL=1",
			},
			InheritEnv: []string{"PATH"},
		}).ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)

	// 3. Host configured with sync source.
	clonesDir := t.TempDir()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		ClonesDir:     clonesDir,
		GitSyncSource: srv.URL,
		GitSyncToken:  expectedToken,
	})
	t.Cleanup(svc.Shutdown)

	// 4. CreateSession with the fake URL — workDirFor clones via the
	// configured sync source. The noop backend manager makes the
	// session creation succeed once the clone lands.
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: fakeURL},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-sync", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 5. The clone in clonesDir must come from the sync server, not
	// from fakeURL (which would have failed the DNS lookup).
	cloneName, err := agent.CloneDirName(fakeURL)
	if err != nil {
		t.Fatalf("CloneDirName: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(clonesDir, cloneName, "synced.txt"))
	if err != nil {
		t.Fatalf("read clone marker: %v", err)
	}
	if string(got) != "via sync source\n" {
		t.Errorf("clone contents wrong: %q", got)
	}
}

// TestWorkDirFor_NoSyncSource_FallsBackToRemoteURL is the laptop-mode
// regression: when GitSyncSource is empty (default), the host clones
// from ref.RemoteURL exactly as before. Catches accidental rewrites.
func TestWorkDirFor_NoSyncSource_FallsBackToRemoteURL(t *testing.T) {
	// Use a local file-system repo as the "remote" so the clone is
	// reproducible without network. file:// URLs are valid clone sources.
	srcRepo := t.TempDir()
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main", srcRepo)
	if err := os.WriteFile(filepath.Join(srcRepo, "marker.txt"), []byte("from origin\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit("-C", srcRepo, "add", "marker.txt")
	runGit("-C", srcRepo, "commit", "-m", "init")

	clonesDir := t.TempDir()
	svc := host.New(host.Options{
		BackendManagers: map[agent.BackendType]agent.BackendManager{
			agent.BackendOpenCode: &noopBackendManager{},
		},
		ClonesDir: clonesDir,
		// GitSyncSource intentionally empty.
	})
	t.Cleanup(svc.Shutdown)

	remoteURL := "file://" + srcRepo
	req := agent.StartRequest{
		Backend: agent.BackendOpenCode,
		GitRef:  agent.GitRef{RemoteURL: remoteURL},
		Prompt:  "hi",
	}
	if _, _, err := svc.CreateSession(context.Background(), "sid-laptop", req); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cloneName, err := agent.CloneDirName(remoteURL)
	if err != nil {
		t.Fatalf("CloneDirName: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(clonesDir, cloneName, "marker.txt"))
	if err != nil {
		t.Fatalf("read clone marker: %v", err)
	}
	if string(got) != "from origin\n" {
		t.Errorf("clone contents wrong: %q", got)
	}
}
