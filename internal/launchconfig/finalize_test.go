package launchconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupOutputPath(t *testing.T) {
	t.Parallel()

	paths := Paths{
		ProjectRoot: "/work/project",
		Project:     "/work/project/.clank/launch.yaml",
		Host:        "/host/preview-launch/project.yaml",
	}
	tests := []struct {
		name  string
		scope Scope
		want  string
	}{
		{name: "project", scope: ScopeProject, want: paths.Project},
		{name: "host staging", scope: ScopeHost, want: "/work/project/.clank/launch.setup.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SetupOutputPath(paths, tt.scope)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("SetupOutputPath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFinalizeSetupInstallsValidatedHostConfig(t *testing.T) {
	root := newLaunchRepo(t)
	t.Setenv("CLANK_DIR", t.TempDir())
	paths, err := ResolvePaths(root)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := SetupOutputPath(paths, ScopeHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, []byte(validLaunchYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := FinalizeSetup(root, ScopeHost)
	if err != nil {
		t.Fatalf("FinalizeSetup: %v", err)
	}
	if resolved.Source.Scope != ScopeHost || resolved.Source.Path != paths.Host {
		t.Fatalf("Source = %+v", resolved.Source)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file remains: %v", err)
	}
	got, err := os.ReadFile(paths.Host)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != validLaunchYAML {
		t.Fatalf("host config = %q", got)
	}
	if info, err := os.Stat(paths.Host); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("host config mode = %v, err = %v", info.Mode().Perm(), err)
	}
	if _, err := os.Stat(filepath.Dir(staging)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty staging directory remains: %v", err)
	}
}

func TestFinalizeSetupKeepsInvalidCandidateForCorrection(t *testing.T) {
	root := newLaunchRepo(t)
	t.Setenv("CLANK_DIR", t.TempDir())
	paths, err := ResolvePaths(root)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := SetupOutputPath(paths, ScopeHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, []byte("prevews: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = FinalizeSetup(root, ScopeHost)
	if err == nil || !strings.Contains(err.Error(), "field prevews not found") {
		t.Fatalf("FinalizeSetup error = %v", err)
	}
	if _, statErr := os.Stat(staging); statErr != nil {
		t.Fatalf("invalid candidate was removed: %v", statErr)
	}
	if _, statErr := os.Stat(paths.Host); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid host config was installed: %v", statErr)
	}
}

func TestFinalizeSetupPreservesNonEmptyProjectClankDirectory(t *testing.T) {
	root := newLaunchRepo(t)
	t.Setenv("CLANK_DIR", t.TempDir())
	paths, err := ResolvePaths(root)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := SetupOutputPath(paths, ScopeHost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(filepath.Dir(staging), "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, []byte(validLaunchYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := FinalizeSetup(root, ScopeHost); err != nil {
		t.Fatalf("FinalizeSetup: %v", err)
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "keep" {
		t.Fatalf("unrelated .clank file = %q, err = %v", got, err)
	}
}

func TestFinalizeSetupLeavesSharedProjectConfigInPlace(t *testing.T) {
	root := newLaunchRepo(t)
	t.Setenv("CLANK_DIR", t.TempDir())
	writeProjectLaunch(t, root, validLaunchYAML)

	resolved, err := FinalizeSetup(root, ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Source.Scope != ScopeProject {
		t.Fatalf("Source = %+v", resolved.Source)
	}
	if _, err := os.Stat(filepath.Join(root, ProjectRelativePath)); err != nil {
		t.Fatalf("project config missing: %v", err)
	}
}
