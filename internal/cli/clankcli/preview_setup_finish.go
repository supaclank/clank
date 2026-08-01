package clankcli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/config"
	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/launchconfig"
)

func inspectPreviewSetup(projectRoot string, scope launchconfig.Scope, out io.Writer) (string, *launchconfig.Resolved, error) {
	resolved, err := launchconfig.FinalizeSetup(projectRoot, scope)
	if err == nil {
		return "", resolved, nil
	}
	paths, pathErr := launchconfig.ResolvePaths(projectRoot)
	if pathErr != nil {
		return "", nil, pathErr
	}
	outputPath, pathErr := launchconfig.SetupOutputPath(paths, scope)
	if pathErr != nil {
		return "", nil, pathErr
	}
	fmt.Fprintf(out, "Generated preview config needs one correction: %v\n", err)
	return fmt.Sprintf(`Clank validation failed for the generated preview configuration at %q:

%s

This remains a non-interactive task. Do not ask questions or wait for input.
Correct only that configuration file, finish within one minute, and report the
path written. Do not install dependencies, start services, commit, push, or open
a pull request.`, outputPath, err), nil, nil
}

func previewSetupSessionError(projectRoot, sessionID string, setupErr error) error {
	if err := config.SetLastSessionForCwd(projectRoot, sessionID); err != nil {
		return fmt.Errorf("preview setup failed: %w (setup session %s; also failed to record it for `clank`: %v)", setupErr, sessionID, err)
	}
	return fmt.Errorf("preview setup failed: %w (setup session %s; run `clank` to inspect it)", setupErr, sessionID)
}

func completePreviewSetupSession(client *daemonclient.Client, sessionID string, out io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Session(sessionID).SetVisibility(ctx, agent.VisibilityDone); err != nil {
		fmt.Fprintf(out, "warning: preview setup succeeded but its agent session could not be marked done: %v\n", err)
	}
}
