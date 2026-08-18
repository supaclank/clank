package clankcli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	daemonclient "github.com/supaclank/clank/internal/daemonclient"
	"github.com/supaclank/clank/internal/host/preview"
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
	out io.Writer,
) (*daemonclient.PreviewStatus, error) {
	if out == nil {
		return nil, fmt.Errorf("preview readiness output is required")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("preview readiness timeout must be positive")
	}
	isReady, err := previewReadyState(status)
	if err != nil {
		return nil, previewStartupError(client, err)
	}
	if isReady {
		return status, nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	statusTicker := time.NewTicker(previewReadyPollInterval)
	defer statusTicker.Stop()
	logTicker := time.NewTicker(previewLogPollInterval)
	defer logTicker.Stop()
	logStream := previewStartupLogStream{out: out}
	defer logStream.finish()

	// Log reads run on their own goroutine (at most one in-flight) so a slow
	// one can never stall statusTicker, which is the readiness source of
	// truth.
	logPoller := newPreviewLogPoller(&logStream, client.Logs)
	logPoller.Poll(ctx)

	for {
		isReady, err = previewReadyState(status)
		if err != nil {
			if streamErr := logPoller.Flush(ctx); streamErr != nil {
				return nil, streamErr
			}
			if logStream.hasOutput {
				return nil, err
			}
			return nil, previewStartupError(client, err)
		}
		if isReady {
			if err := logPoller.Flush(ctx); err != nil {
				return nil, err
			}
			return status, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			err := fmt.Errorf("dev server did not satisfy its configured readiness probe within %s", timeout)
			if streamErr := logPoller.Flush(ctx); streamErr != nil {
				return nil, streamErr
			}
			if logStream.hasOutput {
				return nil, err
			}
			return nil, previewStartupError(client, err)
		case <-logTicker.C:
			logPoller.Poll(ctx)
		case res := <-logPoller.C:
			logPoller.Ack()
			if err := logStream.apply(res); err != nil {
				return nil, err
			}
		case <-statusTicker.C:
			// TODO(ai-review): a status request straddling `timeout` can report
			// ready after the deadline instead of timing out.
			// https://github.com/supaclank/clank/pull/210#discussion_r3696464957
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
