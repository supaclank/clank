package clankcli

import (
	"context"
	"fmt"
	"strings"
	"time"

	daemonclient "github.com/acksell/clank/internal/daemonclient"
	"github.com/acksell/clank/internal/host/preview"
)

const (
	previewReadyPollInterval = 200 * time.Millisecond
	previewStatusReadTimeout = 5 * time.Second
	previewLogReadTimeout    = 3 * time.Second
	previewErrorLogMaxRunes  = 4000
)

func waitPreviewReady(
	ctx context.Context,
	client *daemonclient.PreviewClient,
	status *daemonclient.PreviewStatus,
	timeout time.Duration,
) (*daemonclient.PreviewStatus, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("preview readiness timeout must be positive")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(previewReadyPollInterval)
	defer ticker.Stop()

	for {
		isReady, err := previewReadyState(status)
		if err != nil {
			return nil, previewStartupError(client, err)
		}
		if isReady {
			return status, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, previewStartupError(client, fmt.Errorf("dev server did not satisfy its configured readiness probe within %s", timeout))
		case <-ticker.C:
			statusCtx, cancel := context.WithTimeout(ctx, previewStatusReadTimeout)
			status, err = client.Status(statusCtx)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("read preview startup status: %w", err)
			}
		}
	}
}

func previewReadyState(status *daemonclient.PreviewStatus) (bool, error) {
	if status == nil {
		return false, fmt.Errorf("preview startup returned no status")
	}
	switch preview.State(status.State) {
	case preview.StateReady:
		return true, nil
	case preview.StateStarting:
		return false, nil
	case preview.StateFailed:
		if status.LastError == "" {
			return false, fmt.Errorf("dev server failed during startup")
		}
		return false, fmt.Errorf("dev server failed during startup: %s", status.LastError)
	case preview.StateStopped:
		return false, fmt.Errorf("dev server stopped during startup")
	default:
		return false, fmt.Errorf("dev server returned unknown startup state %q", status.State)
	}
}

func previewStartupError(client *daemonclient.PreviewClient, startupErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), previewLogReadTimeout)
	defer cancel()
	logs, err := client.Logs(ctx)
	if err != nil {
		return fmt.Errorf("%w (also failed to read preview logs: %v)", startupErr, err)
	}
	logText := strings.TrimSpace(string(logs))
	if logText == "" {
		return startupErr
	}
	runes := []rune(logText)
	if len(runes) > previewErrorLogMaxRunes {
		logText = "…" + string(runes[len(runes)-previewErrorLogMaxRunes:])
	}
	return fmt.Errorf("%w\n\nDev server logs:\n%s", startupErr, logText)
}
