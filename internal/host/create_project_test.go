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
)

// makeTemplateRepo builds a real git repo to act as a clone source (the
// "template"), with a couple of files and its own history + origin. It
// returns a file:// clone URL. Cloning from this exercises the real git
// clone path without touching the network.
func makeTemplateRepo(t *testing.T) (cloneURL string, files []string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.email", "tmpl@tmpl")
	run("git", "config", "user.name", "Template")
	run("git", "remote", "add", "origin", "https://example.com/template.git")
	files = []string{"App.tsx", "package.json"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("// "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "template seed")
	return "file://" + dir, files
}

func TestCreateProjectFromTemplate(t *testing.T) {
	cloneURL, files := makeTemplateRepo(t)

	workRoot := filepath.Join(t.TempDir(), "work")
	prev := host.SetWorkRootForTest(workRoot)
	t.Cleanup(func() { host.SetWorkRootForTest(prev) })

	svc := newTestService(t)
	const name = "my-cool-app"
	res, err := svc.CreateProjectFromTemplate(context.Background(), cloneURL, name)
	if err != nil {
		t.Fatalf("CreateProjectFromTemplate: %v", err)
	}

	if res.WorktreeID == "" {
		t.Fatal("empty worktree id")
	}
	if res.Branch != "main" {
		t.Errorf("branch = %q, want main", res.Branch)
	}
	if res.DisplayName != name || res.OriginRepo != name {
		t.Errorf("display=%q origin=%q, want both %q", res.DisplayName, res.OriginRepo, name)
	}
	wantDir := filepath.Join(workRoot, res.WorktreeID)
	if res.WorktreeDir != wantDir {
		t.Errorf("worktree dir = %q, want %q", res.WorktreeDir, wantDir)
	}

	// Template files were materialized.
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(wantDir, f)); err != nil {
			t.Errorf("template file %q missing: %v", f, err)
		}
	}

	// Fresh history: exactly one commit, none of the template's.
	out, err := exec.Command("git", "-C", wantDir, "rev-list", "--count", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-list: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Errorf("commit count = %q, want 1 (template history should be dropped)", got)
	}

	// No remote — the new project is local-only.
	remotes, err := git.RemoteURLs(wantDir)
	if err != nil {
		t.Fatalf("RemoteURLs: %v", err)
	}
	if len(remotes) != 0 {
		t.Errorf("remotes = %v, want none", remotes)
	}

	// Worktree-id stamp lets sessions resolve the dir by id alone.
	stamped, err := agent.ReadLocalWorktreeID(wantDir)
	if err != nil {
		t.Fatalf("ReadLocalWorktreeID: %v", err)
	}
	if stamped != res.WorktreeID {
		t.Errorf("stamped id = %q, want %q", stamped, res.WorktreeID)
	}
}

func TestCreateProjectFromTemplate_Validation(t *testing.T) {
	svc := newTestService(t)
	cases := []struct {
		name     string
		cloneURL string
		project  string
	}{
		{"empty clone url", "", "app"},
		{"empty name", "file:///tmp/whatever", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateProjectFromTemplate(context.Background(), tc.cloneURL, tc.project)
			if !errors.Is(err, host.ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
}
