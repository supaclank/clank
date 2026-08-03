package clankcli

import (
	"fmt"
	"strings"

	"github.com/supaclank/clank/internal/launchconfig"
)

func previewLaunchName(args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	if len(args) != 1 {
		return "", fmt.Errorf("managed preview accepts at most one configured preview name")
	}
	name := strings.TrimSpace(args[0])
	if name == "" {
		return "", fmt.Errorf("preview name must not be empty")
	}
	return name, nil
}

func managedPreviewProjectDir(invokedDir string, isProjectExplicit bool) (string, error) {
	if isProjectExplicit {
		return invokedDir, nil
	}
	paths, err := launchconfig.ResolvePaths(invokedDir)
	if err != nil {
		return "", err
	}
	return paths.ProjectRoot, nil
}
