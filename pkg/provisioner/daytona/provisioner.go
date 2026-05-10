// Package daytona implements provisioner.Provisioner using Daytona's
// hosted control plane: one persistent sandbox per (userID, "daytona")
// tuple, recorded in the SQL store. EnsureHost is idempotent across
// daemon restarts — woken from stopped/archived state if needed, and
// the preview URL/token are refreshed since they rotate across
// stop/start cycles.
//
// TODO/Future: refactor to consume a Daytona-backed sandbox-pool
// package whose API mirrors fly.io sprites — keyed GetOrCreate,
// stable URL, wake-on-traffic. The flyio provisioner is already that
// shape; daytona drags the lower-level state machine inline. Needs
// research: how much can use Daytona's name lookup + auto-archive
// directly, vs. needing a wrapper layer to emulate persistence. Once
// that lands, this provisioner shrinks to a thin clank-host
// configurator on top of the pool, and persistence-across-DB-loss
// becomes a property of the pool's name lookup rather than something
// the provisioner has to reason about.
package daytona

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	apiclient "github.com/daytonaio/daytona/libs/api-client-go"
	"github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	dyterrors "github.com/daytonaio/daytona/libs/sdk-go/pkg/errors"
	sdkopts "github.com/daytonaio/daytona/libs/sdk-go/pkg/options"
	"github.com/daytonaio/daytona/libs/sdk-go/pkg/types"
	"github.com/oklog/ulid/v2"

	hostclient "github.com/acksell/clank/internal/host/client"
	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
)

// Options configures the Daytona provisioner.
type Options struct {
	// APIKey is the Daytona API key. Required when SDKClient is nil.
	APIKey string

	// Snapshot is the name of a Daytona-side snapshot to spawn the
	// sandbox from. Pre-warmed snapshots boot in ~hundreds of ms vs.
	// several seconds for cold OCI image pulls.
	//
	// Exactly one of Snapshot or Image must be set.
	Snapshot string

	// Image is the OCI image Daytona will pull. See Snapshot for the
	// mutual-exclusion contract.
	Image string

	// MirrorBaseURL is the externally-reachable URL the sandbox clones
	// user code from on wake. Required. Historically named
	// "HubBaseURL" — the "hub" layer is gone but the env-var name
	// (CLANK_HUB_URL) is kept to avoid breaking sandbox images
	// mid-migration.
	MirrorBaseURL string

	// MirrorAuthToken is the bearer token paired with MirrorBaseURL.
	MirrorAuthToken string

	// ExtraEnv is forwarded into the sandbox. Keys colliding with
	// reservedSandboxEnv are rejected at New time.
	ExtraEnv map[string]string

	// APIUrl overrides the default Daytona control-plane endpoint.
	APIUrl string

	// OrganizationID scopes operations to a specific Daytona org.
	OrganizationID string

	// ProvisionTimeout caps how long EnsureHost waits for sandbox
	// readiness. Default: 5 minutes.
	ProvisionTimeout time.Duration

	// Resources optionally pins CPU/memory/disk. nil uses Daytona
	// defaults.
	Resources *types.Resources

	// SDKClient overrides the daytona.Client constructor (tests
	// inject; production passes nil).
	SDKClient *daytona.Client
}

// Provisioner manages a single persistent Daytona sandbox per
// (userID, "daytona") tuple. Implements provisioner.Provisioner.
type Provisioner struct {
	opts   Options
	log    *log.Logger
	client *daytona.Client
	store  hoststore.HostStore

	// keyMu serializes EnsureHost calls per userID. Two parallel
	// callers see the same single sandbox instead of racing to
	// create two.
	keyMuMap sync.Mutex
	keyMu    map[string]*sync.Mutex

	// cache maps userID → most-recent (sandbox, client) so subsequent
	// calls don't repeat the resolve/wake/refresh work for an
	// already-healthy host. Invalidated on probe failure.
	cacheMu sync.Mutex
	cache   map[string]*cachedHost
}

type cachedHost struct {
	sandbox   *daytona.Sandbox
	client    *hostclient.HTTP
	transport http.RoundTripper
	hostID    string
	hostname  string
	url       string
	token     string // preview-token (provider-edge layer)
	authToken string // clank-host bearer-token (universal app layer)
}

// New constructs a Provisioner. Returns an error if required options
// are missing or the SDK client fails to initialize. The HostStore is
// the persistence boundary — see pkg/provisioner/hoststore.
func New(opts Options, st hoststore.HostStore, lg *log.Logger) (*Provisioner, error) {
	if st == nil {
		return nil, fmt.Errorf("daytona provisioner: store is required")
	}
	if opts.MirrorBaseURL == "" {
		return nil, fmt.Errorf("daytona provisioner: MirrorBaseURL is required")
	}
	if opts.MirrorAuthToken == "" {
		return nil, fmt.Errorf("daytona provisioner: MirrorAuthToken is required")
	}
	switch {
	case opts.Snapshot == "" && opts.Image == "":
		return nil, fmt.Errorf("daytona provisioner: one of Snapshot or Image must be set")
	case opts.Snapshot != "" && opts.Image != "":
		return nil, fmt.Errorf("daytona provisioner: Snapshot and Image are mutually exclusive (got Snapshot=%q, Image=%q)", opts.Snapshot, opts.Image)
	}
	if err := validateExtraEnv(opts.ExtraEnv); err != nil {
		return nil, fmt.Errorf("daytona provisioner: %w", err)
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
			return nil, fmt.Errorf("daytona provisioner: APIKey is required (or pass an SDKClient for tests)")
		}
		var err error
		c, err = daytona.NewClientWithConfig(&types.DaytonaConfig{
			APIKey:         opts.APIKey,
			APIUrl:         opts.APIUrl,
			OrganizationID: opts.OrganizationID,
		})
		if err != nil {
			return nil, fmt.Errorf("daytona provisioner: build SDK client: %w", err)
		}
	}

	return &Provisioner{
		opts:   opts,
		log:    lg,
		client: c,
		store:  st,
		keyMu:  map[string]*sync.Mutex{},
		cache:  map[string]*cachedHost{},
	}, nil
}

// Stop is a no-op cleanup hook today: the persistent sandbox lives
// past daemon shutdown by design. Future cooperative-suspend behavior
// is invoked explicitly via SuspendHost, not on daemon exit.
func (p *Provisioner) Stop() {}

// EnsureHost implements provisioner.Provisioner.
func (p *Provisioner) EnsureHost(ctx context.Context, userID string) (provisioner.HostRef, error) {
	if userID == "" {
		return provisioner.HostRef{}, fmt.Errorf("daytona provisioner: userID is required")
	}
	mu := p.userMutex(userID)
	mu.Lock()
	defer mu.Unlock()

	// Detach from the caller's cancellation. A daytona cold-create or
	// resume can take 10-90s; if the TUI's request context times out
	// the work would abort partway, the cache stays empty, and the
	// next request restarts from scratch. With WithoutCancel, the
	// in-flight provision finishes (or times out via ProvisionTimeout)
	// and subsequent callers hit the cache.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.opts.ProvisionTimeout)
	defer cancel()

	// Fast path: in-memory cache from a previous EnsureHost in this
	// process. Probe /status before trusting it — provider may have
	// cycled the sandbox out of band.
	if c := p.cacheGet(userID); c != nil {
		probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := c.client.Status(probeCtx)
		probeCancel()
		if err == nil {
			return p.refToHost(c), nil
		}
		// Cached client is stale — drop it and continue to the slow path.
		p.log.Printf("daytona provisioner: cached client probe failed for user %s (%v); refreshing", userID, err)
		p.cacheDrop(userID)
	}

	// Resolve the sandbox: from store, from labels, or by creating.
	// Cold-create path returns the freshly-minted auth-token (already
	// baked into the sandbox env); reuse paths return empty and the
	// real token is read from the store below.
	sandbox, isNew, mintedAuthToken, err := p.resolveOrCreate(ctx, userID)
	if err != nil {
		return provisioner.HostRef{}, err
	}

	// Wake to STARTED. Surfaces error states explicitly.
	if err := p.ensureStarted(ctx, sandbox); err != nil {
		// Best-effort: surface entrypoint logs to aid debugging.
		if logs := fetchEntrypointLogs(sandbox); logs != "" {
			err = fmt.Errorf("%w\n--- sandbox entrypoint logs ---\n%s\n--- end logs ---", err, logs)
		}
		return provisioner.HostRef{}, err
	}

	// Refresh preview link (URLs may rotate across stop/start) and
	// confirm clank-host is actually answering on /status.
	preview, err := getPreviewLinkWithRetry(ctx, sandbox, HostPort)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("get preview link: %w", err)
	}
	if preview.URL == "" || preview.Token == "" {
		return provisioner.HostRef{}, fmt.Errorf("preview link missing url or token: %+v", preview)
	}

	// Capability-token: cold-create path passed it down via
	// mintedAuthToken; reuse path reads from the store row.
	authToken := mintedAuthToken
	if !isNew {
		row, err := p.store.GetHostByUser(ctx, userID, "daytona")
		if err == nil {
			authToken = row.AuthToken
		} else if !errors.Is(err, hoststore.ErrHostNotFound) {
			return provisioner.HostRef{}, fmt.Errorf("read auth-token: %w", err)
		}
	}

	transport, err := chainTransport(authToken, preview.Token, preview.URL)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("build transport: %w", err)
	}
	client := hostclient.NewHTTP(preview.URL, transport)
	if err := waitForHostReady(ctx, client, sandbox.ID); err != nil {
		_ = client.Close()
		if logs := fetchEntrypointLogs(sandbox); logs != "" {
			err = fmt.Errorf("%w\n--- sandbox entrypoint logs ---\n%s\n--- end logs ---", err, logs)
		}
		return provisioner.HostRef{}, fmt.Errorf("wait for clank-host: %w", err)
	}

	hostname := "daytona-" + safeHostnameSuffix(sandbox.ID)

	// Persist the latest known-good URL/token. On a fresh Create we
	// also need the row in the first place.
	hostID, err := p.persistRow(ctx, userID, sandbox.ID, string(hostname), preview.URL, preview.Token, authToken, isNew)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("persist host row: %w", err)
	}

	cached := &cachedHost{
		sandbox:   sandbox,
		client:    client,
		transport: transport,
		hostID:    hostID,
		hostname:  hostname,
		url:       preview.URL,
		token:     preview.Token,
		authToken: authToken,
	}
	p.cacheSet(userID, cached)
	return p.refToHost(cached), nil
}

func (p *Provisioner) refToHost(c *cachedHost) provisioner.HostRef {
	return provisioner.HostRef{
		HostID:    c.hostID,
		URL:       c.url,
		Transport: c.transport,
		AuthToken: c.authToken,
		AutoWake:  false,
		Hostname:  c.hostname,
	}
}

// resolveOrCreate returns the persistent sandbox for this userID,
// creating it if necessary. The bool indicates whether the sandbox
// was newly created (so persistRow knows to set CreatedAt). The
// string return is the freshly-minted auth-token on the cold-create
// path; empty on reuse paths.
func (p *Provisioner) resolveOrCreate(ctx context.Context, userID string) (*daytona.Sandbox, bool, string, error) {
	row, err := p.store.GetHostByUser(ctx, userID, "daytona")
	if err == nil {
		// Try to fetch by ID. NotFound → out-of-band delete.
		sandbox, fetchErr := p.client.Get(ctx, row.ExternalID)
		if fetchErr == nil {
			return sandbox, false, "", nil
		}
		var notFound *dyterrors.DaytonaNotFoundError
		if errors.As(fetchErr, &notFound) {
			p.log.Printf("daytona provisioner: sandbox %s for user %s not found upstream (out-of-band delete?); recreating", row.ExternalID, userID)
			if delErr := p.store.DeleteHostByUser(ctx, userID, "daytona"); delErr != nil {
				p.log.Printf("daytona provisioner: clear stale row: %v", delErr)
			}
			// fall through to create
		} else {
			return nil, false, "", fmt.Errorf("get sandbox %s: %w", row.ExternalID, fetchErr)
		}
	} else if !errors.Is(err, hoststore.ErrHostNotFound) {
		return nil, false, "", fmt.Errorf("look up host: %w", err)
	}

	// Store miss: mint a fresh sandbox. The store row is the sole
	// source of truth for user → sandbox; we don't query Daytona by
	// metadata to "find" anything, ever. If a true persistent-across-
	// DB-loss story is ever needed, that's a snapshot/checkpoint
	// abstraction layer's job, not the provisioner's.
	authToken, err := generateAuthToken()
	if err != nil {
		return nil, false, "", fmt.Errorf("generate auth-token: %w", err)
	}
	sandbox, err := p.create(ctx, authToken)
	if err != nil {
		return nil, false, "", err
	}
	return sandbox, true, authToken, nil
}

// create issues a fresh Daytona sandbox. AutoDeleteInterval is left
// nil so the sandbox persists indefinitely. authToken is baked into
// the sandbox env so clank-host's bearer middleware enforces it from
// the first request. The store row is the sole source of truth for
// user → sandbox binding.
func (p *Provisioner) create(ctx context.Context, authToken string) (*daytona.Sandbox, error) {
	envVars := map[string]string{
		"CLANK_HUB_URL":         p.opts.MirrorBaseURL,
		"CLANK_HUB_TOKEN":       p.opts.MirrorAuthToken,
		"CLANK_HOST_PORT":       fmt.Sprintf("%d", HostPort),
		"CLANK_HOST_AUTH_TOKEN": authToken,
	}
	for k, v := range p.opts.ExtraEnv {
		if v == "" {
			continue
		}
		envVars[k] = v
	}

	base := types.SandboxBaseParams{
		EnvVars: envVars,
		Public:  true, // expose preview port; preview token still gates auth
	}

	startCreate := time.Now()
	createOpts := []func(*sdkopts.CreateSandbox){sdkopts.WithWaitForStart(false)}
	var sandbox *daytona.Sandbox
	var err error
	if p.opts.Snapshot != "" {
		sandbox, err = p.client.Create(ctx, types.SnapshotParams{
			SandboxBaseParams: base,
			Snapshot:          p.opts.Snapshot,
		}, createOpts...)
	} else {
		// Set ENTRYPOINT explicitly: Daytona drops base-image ENTRYPOINT
		// on `FROM <image>` wrapping. Path mirrors cmd/clank-host/Dockerfile.
		img := daytona.Base(p.opts.Image).
			Entrypoint([]string{"/usr/local/bin/entrypoint.sh"})
		sandbox, err = p.client.Create(ctx, types.ImageParams{
			SandboxBaseParams: base,
			Image:             img,
			Resources:         p.opts.Resources,
		}, createOpts...)
	}
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	p.log.Printf("daytona provisioner: sandbox %s created in %s (snapshot=%q image=%q)",
		sandbox.ID, time.Since(startCreate).Round(time.Millisecond), p.opts.Snapshot, p.opts.Image)
	return sandbox, nil
}

// ensureStarted blocks until the sandbox is in the STARTED state or
// returns a useful error. Intermediate states transition via
// WaitForStart; STOPPED triggers Start; terminal-error states fail
// loudly so the user sees the problem.
func (p *Provisioner) ensureStarted(ctx context.Context, sandbox *daytona.Sandbox) error {
	switch sandbox.State {
	case apiclient.SANDBOXSTATE_STARTED:
		return nil

	case apiclient.SANDBOXSTATE_STOPPED, apiclient.SANDBOXSTATE_ARCHIVED:
		p.log.Printf("daytona provisioner: sandbox %s state=%s; starting", sandbox.ID, sandbox.State)
		if err := sandbox.Start(ctx); err != nil {
			return fmt.Errorf("start sandbox %s: %w", sandbox.ID, err)
		}
		return nil

	case apiclient.SANDBOXSTATE_STOPPING:
		// Wait for stop to finish before issuing Start to avoid races.
		if err := sandbox.WaitForStop(ctx, p.opts.ProvisionTimeout); err != nil {
			return fmt.Errorf("wait for stop %s: %w", sandbox.ID, err)
		}
		if err := sandbox.Start(ctx); err != nil {
			return fmt.Errorf("start sandbox %s after stop: %w", sandbox.ID, err)
		}
		return nil

	case apiclient.SANDBOXSTATE_STARTING,
		apiclient.SANDBOXSTATE_RESTORING,
		apiclient.SANDBOXSTATE_CREATING,
		apiclient.SANDBOXSTATE_PULLING_SNAPSHOT:
		if err := sandbox.WaitForStart(ctx, p.opts.ProvisionTimeout); err != nil {
			return fmt.Errorf("wait for start %s (state=%s): %w", sandbox.ID, sandbox.State, err)
		}
		return nil

	case apiclient.SANDBOXSTATE_ERROR,
		apiclient.SANDBOXSTATE_BUILD_FAILED,
		apiclient.SANDBOXSTATE_DESTROYED,
		apiclient.SANDBOXSTATE_DESTROYING:
		return fmt.Errorf("sandbox %s is in terminal state %s; cannot wake", sandbox.ID, sandbox.State)

	default:
		// Unknown future state: treat as needing wake. WaitForStart
		// surfaces a real error if Daytona reaches a terminal state
		// we didn't enumerate.
		p.log.Printf("daytona provisioner: sandbox %s unknown state %s; waiting for start", sandbox.ID, sandbox.State)
		if err := sandbox.WaitForStart(ctx, p.opts.ProvisionTimeout); err != nil {
			return fmt.Errorf("wait for start %s (state=%s): %w", sandbox.ID, sandbox.State, err)
		}
		return nil
	}
}

// persistRow upserts the host record. CreatedAt is preserved on update.
func (p *Provisioner) persistRow(ctx context.Context, userID, externalID, hostname, url, token, authToken string, isNew bool) (string, error) {
	now := time.Now()

	// On update, keep the existing internal ID. On create, mint a new ULID.
	hostID := ""
	if existing, err := p.store.GetHostByUser(ctx, userID, "daytona"); err == nil {
		hostID = existing.ID
	} else if !errors.Is(err, hoststore.ErrHostNotFound) {
		return "", err
	}
	if hostID == "" {
		hostID = ulid.Make().String()
	}

	rec := hoststore.Host{
		ID:         hostID,
		UserID:     userID,
		Provider:   "daytona",
		ExternalID: externalID,
		Hostname:   hostname,
		Status:     hoststore.HostStatusRunning,
		LastURL:    url,
		LastToken:  token,
		AuthToken:  authToken,
		AutoWake:   false,
		UpdatedAt:  now,
	}
	if isNew {
		rec.CreatedAt = now
	}
	if err := p.store.UpsertHost(ctx, rec); err != nil {
		return "", err
	}
	return hostID, nil
}

// SuspendByUser is a convenience wrapper that resolves the user's
// host_id and calls SuspendHost. Unlike EnsureHost, it does NOT wake
// the sandbox if it's already stopped — it's the right call from
// daemon-shutdown paths where waking a sleeping sandbox just to
// immediately suspend it would be perverse.
//
// Returns nil if no host is recorded for this user (nothing to do).
func (p *Provisioner) SuspendByUser(ctx context.Context, userID string) error {
	row, err := p.store.GetHostByUser(ctx, userID, "daytona")
	if errors.Is(err, hoststore.ErrHostNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up host for user %s: %w", userID, err)
	}
	return p.SuspendHost(ctx, row.ID)
}

// SuspendHost cooperatively suspends the underlying sandbox. State is
// preserved; a subsequent EnsureHost wakes it.
func (p *Provisioner) SuspendHost(ctx context.Context, hostID string) error {
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		return fmt.Errorf("look up host %s: %w", hostID, err)
	}
	sandbox, err := p.client.Get(ctx, row.ExternalID)
	if err != nil {
		var notFound *dyterrors.DaytonaNotFoundError
		if errors.As(err, &notFound) {
			// Already gone; nothing to suspend. Surface as no-op.
			return nil
		}
		return fmt.Errorf("get sandbox %s: %w", row.ExternalID, err)
	}
	if sandbox.State == apiclient.SANDBOXSTATE_STOPPED || sandbox.State == apiclient.SANDBOXSTATE_ARCHIVED {
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := sandbox.Stop(stopCtx); err != nil {
		return fmt.Errorf("stop sandbox %s: %w", row.ExternalID, err)
	}
	row.Status = hoststore.HostStatusStopped
	row.UpdatedAt = time.Now()
	if err := p.store.UpsertHost(ctx, row); err != nil {
		p.log.Printf("daytona provisioner: update status after suspend %s: %v", hostID, err)
	}
	// Drop in-memory cache so next EnsureHost re-resolves URL/token.
	p.cacheDrop(row.UserID)
	return nil
}

// DestroyHost permanently deletes the underlying sandbox and the
// store row. Out-of-band deletion at the provider is detected inside
// EnsureHost (NotFound from Get) and handled there; callers do not
// need to invoke DestroyHost for that case.
func (p *Provisioner) DestroyHost(ctx context.Context, hostID string) error {
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		return fmt.Errorf("look up host %s: %w", hostID, err)
	}
	sandbox, err := p.client.Get(ctx, row.ExternalID)
	if err == nil {
		delCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if delErr := sandbox.Delete(delCtx); delErr != nil {
			cancel()
			return fmt.Errorf("delete sandbox %s: %w", row.ExternalID, delErr)
		}
		cancel()
	} else {
		var notFound *dyterrors.DaytonaNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("get sandbox %s: %w", row.ExternalID, err)
		}
		// Already gone upstream — proceed to remove the row.
	}
	if err := p.store.DeleteHostByID(ctx, hostID); err != nil {
		return fmt.Errorf("delete host row %s: %w", hostID, err)
	}
	p.cacheDrop(row.UserID)
	return nil
}

// userMutex returns (lazily creating) the per-userID mutex. Two
// concurrent EnsureHost calls for the same user serialize on this
// mutex so they converge on a single sandbox instead of racing two
// Creates.
func (p *Provisioner) userMutex(userID string) *sync.Mutex {
	p.keyMuMap.Lock()
	defer p.keyMuMap.Unlock()
	if mu, ok := p.keyMu[userID]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	p.keyMu[userID] = mu
	return mu
}

func (p *Provisioner) cacheGet(userID string) *cachedHost {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	return p.cache[userID]
}

func (p *Provisioner) cacheSet(userID string, c *cachedHost) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.cache[userID] = c
}

func (p *Provisioner) cacheDrop(userID string) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if c, ok := p.cache[userID]; ok && c.client != nil {
		_ = c.client.Close()
	}
	delete(p.cache, userID)
}
