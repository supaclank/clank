// Package daytona provisions cloud sandboxes via Daytona's hosted
// control plane and registers each one with the hub catalog as a
// `*hostclient.HTTP`.
package daytona

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	"github.com/daytonaio/daytona/libs/sdk-go/pkg/types"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
	hostclient "github.com/acksell/clank/internal/host/client"
)

// HostPort is the TCP port clank-host listens on inside the sandbox.
// Must match cmd/clank-host/Dockerfile's EXPOSE.
const HostPort = 7878

// reservedSandboxEnv keys are populated by the launcher; ExtraEnv must not override them.
var reservedSandboxEnv = []string{"CLANK_HUB_URL", "CLANK_HUB_TOKEN", "CLANK_HOST_PORT"}

// Options configures the Launcher.
type Options struct {
	// APIKey is the Daytona API key. Required when SDKClient is nil.
	APIKey string

	// Image is the OCI image Daytona will pull.
	// Defaults to the published clank-host image.
	Image string

	// HubBaseURL is the externally-reachable URL of the cloud hub. Required.
	HubBaseURL string

	// HubAuthToken is the bearer token paired with HubBaseURL.
	HubAuthToken string

	// ExtraEnv is forwarded into the sandbox. Keys with empty values are dropped.
	ExtraEnv map[string]string

	// APIUrl overrides the default Daytona control-plane endpoint.
	APIUrl string

	// OrganizationID scopes operations to a specific Daytona org.
	OrganizationID string

	// ProvisionTimeout caps how long Launch waits for sandbox readiness. Default: 5 minutes.
	ProvisionTimeout time.Duration

	// Resources optionally pins CPU/memory/disk. nil uses Daytona defaults.
	Resources *types.Resources

	// SDKClient overrides the daytona.Client constructor (tests inject; production passes nil).
	SDKClient *daytona.Client
}

// Launcher provisions Daytona sandboxes on demand. Stop() deletes every
// sandbox created during this process's lifetime.
type Launcher struct {
	opts   Options
	log    *log.Logger
	client *daytona.Client

	// stopping blocks late Launches from leaking sandboxes that finish Create after Stop snapshotted.
	createdMu sync.Mutex
	created   []*daytona.Sandbox
	stopping  bool
}

// New constructs a Launcher. Required options missing returns an error.
func New(opts Options, lg *log.Logger) (*Launcher, error) {
	if opts.HubBaseURL == "" {
		return nil, fmt.Errorf("daytona launcher: HubBaseURL is required")
	}
	if opts.HubAuthToken == "" {
		return nil, fmt.Errorf("daytona launcher: HubAuthToken is required")
	}
	if opts.Image == "" {
		opts.Image = "ghcr.io/acksell/clank-host:latest"
	}
	if opts.ProvisionTimeout == 0 {
		opts.ProvisionTimeout = 5 * time.Minute
	}
	if lg == nil {
		lg = log.Default()
	}

	c := opts.SDKClient
	if c == nil {
		if opts.APIKey == "" {
			return nil, fmt.Errorf("daytona launcher: APIKey is required (or pass an SDKClient for tests)")
		}
		var err error
		c, err = daytona.NewClientWithConfig(&types.DaytonaConfig{
			APIKey:         opts.APIKey,
			APIUrl:         opts.APIUrl,
			OrganizationID: opts.OrganizationID,
		})
		if err != nil {
			return nil, fmt.Errorf("daytona launcher: build SDK client: %w", err)
		}
	}

	return &Launcher{opts: opts, log: lg, client: c}, nil
}

// Launch implements hub.HostLauncher.
func (l *Launcher) Launch(ctx context.Context, _ agent.LaunchHostSpec) (host.Hostname, *hostclient.HTTP, error) {
	ctx, cancel := context.WithTimeout(ctx, l.opts.ProvisionTimeout)
	defer cancel()

	envVars := map[string]string{
		"CLANK_HUB_URL":   l.opts.HubBaseURL,
		"CLANK_HUB_TOKEN": l.opts.HubAuthToken,
		"CLANK_HOST_PORT": fmt.Sprintf("%d", HostPort),
	}
	for k, v := range l.opts.ExtraEnv {
		if v == "" {
			continue
		}
		for _, r := range reservedSandboxEnv {
			if k == r {
				return "", nil, fmt.Errorf("daytona launcher: ExtraEnv key %q is reserved by the launcher", k)
			}
		}
		envVars[k] = v
	}

	// Set ENTRYPOINT explicitly: Daytona drops base-image ENTRYPOINT on
	// `FROM <image>` wrapping (daytonaio/daytona#3853). Path mirrors
	// cmd/clank-host/Dockerfile.
	img := daytona.Base(l.opts.Image).
		Entrypoint([]string{"/usr/local/bin/entrypoint.sh"})

	sandbox, err := l.client.Create(ctx, types.ImageParams{
		SandboxBaseParams: types.SandboxBaseParams{
			EnvVars: envVars,
			// Public=true exposes the preview port; the preview token still gates auth.
			Public: true,
		},
		Image:     img,
		Resources: l.opts.Resources,
	})
	if err != nil {
		return "", nil, fmt.Errorf("create sandbox: %w", err)
	}
	l.createdMu.Lock()
	if l.stopping {
		l.createdMu.Unlock()
		_ = l.deleteBackground(sandbox)
		return "", nil, fmt.Errorf("daytona launcher: stopping; sandbox %s deleted", sandbox.ID)
	}
	l.created = append(l.created, sandbox)
	l.createdMu.Unlock()

	hostname := host.Hostname("daytona-" + safeHostnameSuffix(sandbox.ID))

	preview, err := sandbox.GetPreviewLink(ctx, HostPort)
	if err != nil {
		l.untrackAndDelete(sandbox)
		return "", nil, fmt.Errorf("get preview link: %w", err)
	}
	if preview.URL == "" || preview.Token == "" {
		l.untrackAndDelete(sandbox)
		return "", nil, fmt.Errorf("preview link response missing url or token: %+v", preview)
	}

	transport := &previewTokenInjector{token: preview.Token}
	client := hostclient.NewHTTP(preview.URL, transport)

	if err := l.waitForHostReady(ctx, client, sandbox.ID); err != nil {
		// Surface entrypoint logs in the error so debugging doesn't require sandbox shell access.
		if logs := l.fetchEntrypointLogs(sandbox); logs != "" {
			err = fmt.Errorf("%w\n--- sandbox entrypoint logs ---\n%s\n--- end logs ---", err, logs)
		}
		_ = client.Close()
		l.untrackAndDelete(sandbox)
		return "", nil, fmt.Errorf("wait for clank-host: %w", err)
	}

	l.log.Printf("daytona launcher: sandbox %s ready at %s (host=%s)", sandbox.ID, preview.URL, hostname)
	return hostname, client, nil
}

// fetchEntrypointLogs is best-effort. Returns "" on any failure.
func (l *Launcher) fetchEntrypointLogs(s *daytona.Sandbox) string {
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

// waitForHostReady polls /status until 2xx or ctx expires. Bridges
// the gap between Daytona's "started" state and clank-host actually
// binding its port.
func (l *Launcher) waitForHostReady(ctx context.Context, c *hostclient.HTTP, sandboxID string) error {
	t := time.NewTicker(500 * time.Millisecond)
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

// Stop deletes every sandbox the launcher created. Idempotent.
func (l *Launcher) Stop() {
	l.createdMu.Lock()
	l.stopping = true
	created := l.created
	l.created = nil
	l.createdMu.Unlock()
	for _, s := range created {
		// Per-iteration timeout so one stalled delete doesn't starve the rest.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := s.Delete(ctx)
		cancel()
		if err != nil {
			l.log.Printf("daytona launcher: cleanup %s: %v", s.ID, err)
		}
	}
}

// deleteBackground deletes a sandbox with a fresh ctx; logs errors.
func (l *Launcher) deleteBackground(s *daytona.Sandbox) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Delete(ctx); err != nil {
		l.log.Printf("daytona launcher: cleanup %s: %v", s.ID, err)
		return err
	}
	return nil
}

// untrack removes s from l.created. No-op if s isn't tracked.
func (l *Launcher) untrack(s *daytona.Sandbox) {
	l.createdMu.Lock()
	defer l.createdMu.Unlock()
	for i, c := range l.created {
		if c == s {
			l.created = append(l.created[:i], l.created[i+1:]...)
			return
		}
	}
}

// untrackAndDelete removes s from l.created and deletes it remotely.
// Use on Launch failure paths so failed sandboxes don't pile up in
// the cleanup list (Stop would otherwise issue duplicate deletes).
func (l *Launcher) untrackAndDelete(s *daytona.Sandbox) {
	l.untrack(s)
	_ = l.deleteBackground(s)
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
