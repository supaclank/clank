package clankcli

import (
	"fmt"
	"strings"

	"github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/launchconfig"
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

func previewSetupRequiredError(status *daemonclient.PreviewStatus) error {
	if status == nil || !status.SetupRequired {
		return fmt.Errorf("preview launch setup is required, but the daemon did not return setup instructions")
	}
	return fmt.Errorf(`preview launch setup is required

Ask your connected agent to run the setup task below. It will ask whether to write the shared project config or this host's private config.

%s`, status.SetupPrompt)
}
