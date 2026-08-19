package clankcli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/supaclank/clank/internal/agent"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/launchconfig"
	"github.com/supaclank/clank/internal/tui"
)

const (
	previewSetupTaskTimeout       = 4 * time.Minute
	previewSetupCorrectionTimeout = 90 * time.Second
	previewSetupVisibleLines      = 14
)

func writePreviewSetupNotice(out io.Writer, isHosted bool) {
	location := ""
	if isHosted {
		location = " on the hosted worktree"
	}
	fmt.Fprintf(out, "\nOne-time setup: configuring the web preview with your connected agent%s…\n", location)
	fmt.Fprintf(out, "The agent will create %s and may update the frontend's development-server config to accept Clank's preview origin. It is instructed not to change production behavior.\n\n", launchconfig.ProjectRelativePath)
}

type previewSetupResult struct {
	Launch      *launchconfig.Resolved
	ProjectRoot string
	SessionID   string
}

func runPreviewSetup(
	ctx context.Context,
	client *daemonclient.Client,
	backend agent.BackendType,
	projectDir string,
	in io.Reader,
	out io.Writer,
) (*previewSetupResult, error) {
	paths, err := launchconfig.ResolvePaths(projectDir)
	if err != nil {
		return nil, err
	}
	writePreviewSetupNotice(out, false)

	info, events, cancelEvents, err := createPreviewSetupSession(ctx, client, backend, paths)
	if err != nil {
		return nil, err
	}
	result, err := runPreviewSetupTask(ctx, client, info.ID, events, cancelEvents, in, out, tui.TaskOptions{
		Title:           "One-time preview setup",
		Timeout:         previewSetupTaskTimeout,
		MaxVisibleLines: previewSetupVisibleLines,
	})
	if err != nil {
		return nil, previewSetupSessionError(paths.ProjectRoot, info.ID, err)
	}
	if result.Err != nil {
		return nil, previewSetupSessionError(paths.ProjectRoot, info.ID, result.Err)
	}

	correction, resolved, err := inspectPreviewSetup(paths.ProjectRoot, out)
	if err != nil {
		return nil, previewSetupSessionError(paths.ProjectRoot, info.ID, err)
	}
	if correction != "" {
		result, err = runPreviewSetupCorrection(ctx, client, info.ID, correction, in, out)
		if err != nil {
			return nil, previewSetupSessionError(paths.ProjectRoot, info.ID, err)
		}
		if result.Err != nil {
			return nil, previewSetupSessionError(paths.ProjectRoot, info.ID, result.Err)
		}
		resolved, err = launchconfig.FinalizeSetup(paths.ProjectRoot)
		if err != nil {
			return nil, previewSetupSessionError(paths.ProjectRoot, info.ID, fmt.Errorf("generated configuration is still invalid after one correction: %w", err))
		}
	}

	fmt.Fprintf(out, "Preview configuration generated: %s (default: %s)\n", resolved.Source.Path, resolved.Name)
	return &previewSetupResult{Launch: resolved, ProjectRoot: paths.ProjectRoot, SessionID: info.ID}, nil
}
