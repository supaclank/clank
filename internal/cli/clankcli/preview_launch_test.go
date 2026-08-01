package clankcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acksell/clank/internal/daemonclient"
)

func TestPreviewLaunchName(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{name: "default", want: ""},
		{name: "named", args: []string{"web-app"}, want: "web-app"},
		{name: "too many", args: []string{"web", "admin"}, wantErr: "at most one"},
		{name: "blank", args: []string{"  "}, wantErr: "must not be empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := previewLaunchName(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("previewLaunchName(%q): err = %v, want containing %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("previewLaunchName(%q) = %q, %v; want %q, nil", tt.args, got, err, tt.want)
			}
		})
	}
}

func TestManagedPreviewProjectDir(t *testing.T) {
	t.Setenv("CLANK_DIR", t.TempDir())
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	subdir := filepath.Join(repo, "apps", "web")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	implicit, err := managedPreviewProjectDir(subdir, false)
	if err != nil {
		t.Fatal(err)
	}
	realRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if implicit != realRepo {
		t.Errorf("implicit project dir = %q, want repository root %q", implicit, realRepo)
	}
	explicit, err := managedPreviewProjectDir(subdir, true)
	if err != nil {
		t.Fatal(err)
	}
	if explicit != subdir {
		t.Errorf("explicit project dir = %q, want selected subdirectory %q", explicit, subdir)
	}
}

func TestPreviewSetupRequiredErrorIncludesAgentPrompt(t *testing.T) {
	t.Parallel()

	err := previewSetupRequiredError(&daemonclient.PreviewStatus{
		SetupRequired: true,
		SetupPrompt:   "write one of two configs",
	})
	if !strings.Contains(err.Error(), "connected agent") || !strings.Contains(err.Error(), "write one of two configs") {
		t.Fatalf("previewSetupRequiredError = %q", err)
	}
}
