package clankcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/supaclank/clank/internal/agent"
	"github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/internal/host/preview"
	"github.com/supaclank/clank/internal/tui"
	"github.com/supaclank/clank/pkg/preview/tokens"
)

func runHostedGitHubPullRequestPreview(
	ctx context.Context,
	client *daemonclient.Client,
	worktreeID string,
	launchName string,
	backend string,
	in io.Reader,
	out io.Writer,
) error {
	if err := offerPreviewAgentConnect(ctx, client, backend, in, out); err != nil {
		return err
	}
	backendType, err := resolveBackend(backend, out)
	if err != nil {
		return err
	}

	startCtx, cancel := context.WithTimeout(ctx, previewStartupTimeout)
	defer cancel()
	previewClient := client.Preview(worktreeID)
	if launchName != "" {
		previewClient = previewClient.Named(launchName)
	}
	status, setupSessionID, err := startHostedPreview(startCtx, client, previewClient, backendType, worktreeID, in, out)
	if err != nil {
		return err
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = previewClient.Stop(stopCtx)
	}()
	if status.Kind != string(preview.KindWeb) {
		return fmt.Errorf("hosted pull-request previews require a web launch, got %q", status.Kind)
	}
	if status.ServiceName == "" {
		return fmt.Errorf("hosted preview started without a service name")
	}
	previewClient = client.Preview(worktreeID).Named(status.ServiceName)

	go keepHostedPreviewAlive(ctx, previewClient)
	_, _ = fmt.Fprintln(out, "Waiting for the hosted dev server to come up…")
	status, err = waitPreviewReady(ctx, previewClient, status, previewStartupTimeout, out)
	if err != nil {
		return err
	}
	if setupSessionID != "" {
		completePreviewSetupSession(client, setupSessionID, out)
		_, _ = fmt.Fprintln(out, "One-time preview setup complete.")
	}
	if status.URL == "" || status.Token == "" {
		return fmt.Errorf("the remote host started the app but did not provide a preview URL; configure its preview webhook")
	}
	signed, err := client.SignPreviewToken(ctx, status.Token, tokens.MaxSigTTL)
	if err != nil {
		return fmt.Errorf("authorize hosted preview URL: %w", err)
	}
	if signed.SignedURL == "" {
		return fmt.Errorf("preview URL signing returned an empty URL")
	}

	_, _ = fmt.Fprintf(out, "\n  Preview:  %s\n\nPress Ctrl+C to stop the preview.\n", styleCmdHint.Render(signed.SignedURL))
	_ = openBrowser(signed.SignedURL)
	<-ctx.Done()
	_, _ = fmt.Fprintln(out, "\nShutting down preview…")
	return nil
}

func startHostedPreview(
	ctx context.Context,
	client *daemonclient.Client,
	previewClient *daemonclient.PreviewClient,
	backend agent.BackendType,
	worktreeID string,
	in io.Reader,
	out io.Writer,
) (*daemonclient.PreviewStatus, string, error) {
	_, _ = fmt.Fprintln(out, "Starting the hosted preview dev server…")
	status, err := previewClient.Start(ctx)
	if err == nil {
		return status, "", nil
	}
	var setupRequired *daemonclient.PreviewSetupRequiredError
	if !errors.As(err, &setupRequired) {
		return nil, "", fmt.Errorf("start hosted preview: %w", err)
	}

	sessionID, err := runHostedPreviewSetup(ctx, client, backend, worktreeID, setupRequired.SetupPrompt, in, out)
	if err != nil {
		return nil, sessionID, err
	}
	status, err = previewClient.Start(ctx)
	if err == nil {
		return status, sessionID, nil
	}
	if !errors.As(err, &setupRequired) {
		return nil, sessionID, fmt.Errorf("start hosted preview after setup: %w", err)
	}

	result, correctionErr := runPreviewSetupCorrection(ctx, client, sessionID, setupRequired.SetupPrompt, in, out)
	if correctionErr != nil {
		return nil, sessionID, fmt.Errorf("correct hosted preview setup: %w", correctionErr)
	}
	if result.Err != nil {
		return nil, sessionID, fmt.Errorf("correct hosted preview setup: %w", result.Err)
	}
	status, err = previewClient.Start(ctx)
	if err != nil {
		return nil, sessionID, fmt.Errorf("start hosted preview after correction: %w", err)
	}
	return status, sessionID, nil
}

func runHostedPreviewSetup(
	ctx context.Context,
	client *daemonclient.Client,
	backend agent.BackendType,
	worktreeID string,
	prompt string,
	in io.Reader,
	out io.Writer,
) (string, error) {
	if prompt == "" {
		return "", fmt.Errorf("host requested preview setup without a setup prompt")
	}
	writePreviewSetupNotice(out, true)
	eventsCtx, cancelEvents := context.WithCancel(ctx)
	events, err := client.Sessions().Subscribe(eventsCtx)
	if err != nil {
		cancelEvents()
		return "", fmt.Errorf("subscribe to hosted preview setup: %w", err)
	}

	createCtx, cancelCreate := context.WithTimeout(ctx, createSessionTimeout)
	defer cancelCreate()
	req := newHostedPreviewSetupRequest(backend, worktreeID, prompt)
	req.Config, err = defaultPresetConfig(createCtx, client, backend, req.Hostname)
	if err != nil {
		cancelEvents()
		return "", err
	}
	info, err := client.Sessions().Create(createCtx, req)
	if err != nil {
		cancelEvents()
		return "", fmt.Errorf("create hosted preview setup session: %w", err)
	}
	result, err := runPreviewSetupTask(ctx, client, info.ID, events, cancelEvents, in, out, tui.TaskOptions{
		Title:           "One-time hosted preview setup",
		Timeout:         previewSetupTaskTimeout,
		MaxVisibleLines: previewSetupVisibleLines,
	})
	if err != nil {
		return info.ID, fmt.Errorf("hosted preview setup session %s: %w", info.ID, err)
	}
	if result.Err != nil {
		return info.ID, fmt.Errorf("hosted preview setup session %s: %w", info.ID, result.Err)
	}
	return info.ID, nil
}

func newHostedPreviewSetupRequest(backend agent.BackendType, worktreeID, prompt string) agent.StartRequest {
	return agent.StartRequest{
		Backend:  backend,
		Hostname: host.HostLocal,
		GitRef:   agent.GitRef{WorktreeID: worktreeID},
		Prompt:   prompt,
	}
}

func keepHostedPreviewAlive(ctx context.Context, previewClient *daemonclient.PreviewClient) {
	ticker := time.NewTicker(previewKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, _ = previewClient.Status(statusCtx)
			cancel()
		}
	}
}
