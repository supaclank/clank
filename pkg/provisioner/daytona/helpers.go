package daytona

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	"github.com/daytonaio/daytona/libs/sdk-go/pkg/types"

	hostclient "github.com/acksell/clank/internal/host/client"
)

// HostPort is the TCP port clank-host listens on inside the sandbox.
// Must match cmd/clank-host/Dockerfile's EXPOSE.
const HostPort = 7878

// reservedSandboxEnv keys are populated by the provisioner; ExtraEnv
// must not override them.
var reservedSandboxEnv = []string{
	"CLANK_HOST_PORT",
	"CLANK_HOST_AUTH_TOKEN",
}

// getPreviewLinkWithRetry calls GetPreviewLink with a tight backoff so
// a no-wait Create that hasn't finished provisioning the preview
// routing layer doesn't blow up the launch. The retry budget is
// bounded by the parent ctx.
func getPreviewLinkWithRetry(ctx context.Context, sandbox *daytona.Sandbox, port int) (*types.PreviewLink, error) {
	delay := 50 * time.Millisecond
	const maxDelay = 500 * time.Millisecond
	var lastErr error
	for {
		preview, err := sandbox.GetPreviewLink(ctx, port)
		if err == nil {
			return preview, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("preview link not ready (last error: %v)", lastErr)
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// waitForHostReady polls /status until 2xx or ctx expires. Bridges
// the gap between Daytona's "started" state and clank-host actually
// binding its port.
//
// Poll cadence is tight (100ms) because clank-host is typically up in
// a few hundred ms once Daytona reports the sandbox started — a 500ms
// tick wastes a full interval on average waiting for the next probe.
func waitForHostReady(ctx context.Context, c *hostclient.HTTP, sandboxID string) error {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	var lastErr error
	for {
		// Short per-attempt timeout so proxy 502s return fast.
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := c.Status(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("sandbox %s never reached ready (last error: %v)", sandboxID, lastErr)
			}
			return ctx.Err()
		case <-t.C:
		}
	}
}

// safeHostnameSuffix returns the trailing UUID segment, capped at 12 chars.
func safeHostnameSuffix(id string) string {
	if i := strings.LastIndex(id, "-"); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// fetchEntrypointLogs is best-effort. Returns "" on any failure.
// Surfacing these in launch-failure errors saves debugging trips into
// the sandbox shell.
func fetchEntrypointLogs(s *daytona.Sandbox) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := s.Process.GetEntrypointLogs(ctx)
	if err != nil || resp == nil {
		return ""
	}
	var b strings.Builder
	if resp.Stdout != "" {
		b.WriteString("[stdout]\n")
		b.WriteString(resp.Stdout)
		b.WriteString("\n")
	}
	if resp.Stderr != "" {
		b.WriteString("[stderr]\n")
		b.WriteString(resp.Stderr)
		b.WriteString("\n")
	}
	if b.Len() == 0 && resp.Output != "" {
		b.WriteString(resp.Output)
	}
	return b.String()
}

// validateExtraEnv rejects keys that the provisioner manages itself.
// User overrides for these would silently break launcher wiring.
func validateExtraEnv(extra map[string]string) error {
	for k := range extra {
		for _, r := range reservedSandboxEnv {
			if k == r {
				return fmt.Errorf("ExtraEnv key %q is reserved by the provisioner", k)
			}
		}
	}
	return nil
}

