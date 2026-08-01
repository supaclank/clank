package launchconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFinalizeSetupValidatesProjectConfigInPlace(t *testing.T) {
	t.Parallel()
	root := newLaunchRepo(t)
	writeProjectLaunch(t, root, validLaunchYAML)

	resolved, err := FinalizeSetup(root)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Source.Path != filepath.Join(root, ProjectRelativePath) {
		t.Fatalf("Source = %+v", resolved.Source)
	}
	if _, err := os.Stat(filepath.Join(root, ProjectRelativePath)); err != nil {
		t.Fatalf("project config missing: %v", err)
	}
}

func TestFinalizeSetupKeepsInvalidProjectConfigForCorrection(t *testing.T) {
	t.Parallel()
	root := newLaunchRepo(t)
	writeProjectLaunch(t, root, "prevews: {}\n")

	_, err := FinalizeSetup(root)
	if err == nil || !strings.Contains(err.Error(), "field prevews not found") {
		t.Fatalf("FinalizeSetup error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ProjectRelativePath)); statErr != nil {
		t.Fatalf("invalid project config was removed: %v", statErr)
	}
}
