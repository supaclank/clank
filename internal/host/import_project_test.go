package host_test

import (
	"context"
	"errors"
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

func TestImportProjectFromGitHub_Validation(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	cases := []struct{ name, owner, repo string }{
		{"empty owner", "", "api"},
		{"empty repo", "acme", ""},
		{"owner traversal", "../etc", "api"},
		{"repo with slash", "acme", "a/b"},
		{"repo with space", "acme", "a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ImportProjectFromGitHub(context.Background(), tc.owner, tc.repo)
			if !errors.Is(err, host.ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestImportProjectFromGitHub_NotConnected(t *testing.T) {
	// Not parallel: sets HOME so the GitHub manager reads an empty store.
	t.Setenv("HOME", t.TempDir())
	svc := newTestService(t)

	_, err := svc.ImportProjectFromGitHub(context.Background(), "acme", "api")
	if !errors.Is(err, githubpkg.ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
}

func TestImportProjectFromGitHub_ClonesKeepingRemote(t *testing.T) {
	// Not parallel: t.Setenv(HOME) + the package-global clone-base override.
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A bare repo laid out at <base>/acme/api.git so the import's
	// constructed URL (<base>/acme/api.git) resolves to it.
	srcURL, files := makeTemplateRepo(t)
	srcDir := strings.TrimPrefix(srcURL, "file://")
	reposRoot := t.TempDir()
	bare := filepath.Join(reposRoot, "acme", "api.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "clone", "--bare", srcDir, bare).CombinedOutput(); err != nil {
		t.Fatalf("bare clone: %v\n%s", err, out)
	}
	prevBase := host.SetGitHubCloneBaseForTest("file://" + reposRoot)
	t.Cleanup(func() { host.SetGitHubCloneBaseForTest(prevBase) })

	workRoot := filepath.Join(t.TempDir(), "work")
	prevRoot := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prevRoot) })

	svc := newTestService(t)
	if err := svc.GitHub().Store().Write(githubpkg.Credentials{AccessToken: "gho_test"}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	res, err := svc.ImportProjectFromGitHub(context.Background(), "acme", "api")
	if err != nil {
		t.Fatalf("ImportProjectFromGitHub: %v", err)
	}

	if res.WorktreeID == "" {
		t.Fatal("empty worktree id")
	}
	if res.DisplayName != "api" || res.OriginRepo != "acme/api" {
		t.Errorf("display=%q origin=%q, want api / acme/api", res.DisplayName, res.OriginRepo)
	}
	wantDir := filepath.Join(workRoot, res.WorktreeID)
	if res.WorktreeDir != wantDir {
		t.Errorf("worktree dir = %q, want %q", res.WorktreeDir, wantDir)
	}

	// Repo files materialized.
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(wantDir, f)); err != nil {
			t.Errorf("file %q missing: %v", f, err)
		}
	}

	// Origin remote kept, pointing at the cloned repo.
	gotURL, err := git.RemoteURL(wantDir, "origin")
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	if want := "file://" + bare; gotURL != want {
		t.Errorf("origin url = %q, want %q", gotURL, want)
	}

	// Worktree-id stamped so sessions resolve the dir by id alone.
	stamped, err := agent.ReadLocalWorktreeID(wantDir)
	if err != nil {
		t.Fatalf("ReadLocalWorktreeID: %v", err)
	}
	if stamped != res.WorktreeID {
		t.Errorf("stamped id = %q, want %q", stamped, res.WorktreeID)
	}
}
