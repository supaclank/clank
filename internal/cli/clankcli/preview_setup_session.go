package clankcli

import (
	"context"
	"fmt"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/supaclank/clank/internal/agent"
	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/launchconfig"
	"github.com/supaclank/clank/internal/tui"
)

func createPreviewSetupSession(
	ctx context.Context,
	client *daemonclient.Client,
	backend agent.BackendType,
	paths launchconfig.Paths,
) (*agent.SessionInfo, <-chan agent.Event, context.CancelFunc, error) {
	prompt, err := launchconfig.SetupTaskPrompt(paths)
	if err != nil {
		return nil, nil, nil, err
	}
	eventsCtx, cancelEvents := context.WithCancel(ctx)
	events, err := client.Sessions().Subscribe(eventsCtx)
	if err != nil {
		cancelEvents()
		return nil, nil, nil, fmt.Errorf("subscribe to preview setup progress: %w", err)
	}

	createCtx, cancelCreate := context.WithTimeout(ctx, createSessionTimeout)
	defer cancelCreate()
	req := newStartRequest(backend, paths.ProjectRoot, "", prompt)
	req.Config, err = defaultPresetConfig(createCtx, client, backend, req.Hostname)
	if err != nil {
		cancelEvents()
		return nil, nil, nil, err
	}
	info, err := client.Sessions().Create(createCtx, req)
	if err != nil {
		cancelEvents()
		return nil, nil, nil, fmt.Errorf("create preview setup session: %w", err)
	}
	return info, events, cancelEvents, nil
}

func runPreviewSetupCorrection(
	ctx context.Context,
	client *daemonclient.Client,
	sessionID string,
	prompt string,
	in io.Reader,
	out io.Writer,
) (tui.TaskResult, error) {
	eventsCtx, cancelEvents := context.WithCancel(ctx)
	events, err := client.Sessions().Subscribe(eventsCtx)
	if err != nil {
		cancelEvents()
		return tui.TaskResult{}, fmt.Errorf("subscribe to preview setup correction: %w", err)
	}
	sendCtx, cancelSend := context.WithTimeout(ctx, 30*time.Second)
	err = client.Session(sessionID).Send(sendCtx, agent.SendMessageOpts{Text: prompt})
	cancelSend()
	if err != nil {
		cancelEvents()
		return tui.TaskResult{}, fmt.Errorf("send preview setup correction: %w", err)
	}
	return runPreviewSetupTask(ctx, client, sessionID, events, cancelEvents, in, out, tui.TaskOptions{
		Title:           "Correcting preview setup",
		Timeout:         previewSetupCorrectionTimeout,
		MaxVisibleLines: previewSetupVisibleLines,
	})
}

func runPreviewSetupTask(
	ctx context.Context,
	client *daemonclient.Client,
	sessionID string,
	events <-chan agent.Event,
	cancelEvents context.CancelFunc,
	in io.Reader,
	out io.Writer,
	options tui.TaskOptions,
) (tui.TaskResult, error) {
	tui.ApplyPreferredTheme()
	model, err := tui.NewSessionTaskModel(client, sessionID, options)
	if err != nil {
		cancelEvents()
		return tui.TaskResult{}, err
	}
	model.SetEventChannel(events, cancelEvents)

	cleanupLogs := redirectLogToFile()
	defer cleanupLogs()
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	if _, err := program.Run(); err != nil {
		cancelEvents()
		return tui.TaskResult{}, fmt.Errorf("show preview setup progress: %w", err)
	}
	return model.Result(), nil
}
