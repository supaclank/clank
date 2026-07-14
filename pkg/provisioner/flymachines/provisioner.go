// Package flymachines implements provisioner.Provisioner on raw Fly
// Machines: one Fly app per user on its own private network (the app
// is the tenant-isolation boundary — machines in one app share 6PN),
// holding exactly one machine and one volume.
//
// Reachability: machines have no public IPs. Each app gets a Flycast
// address allocated on the GATEWAY's network, so the gateway can dial
// in (and fly-proxy autostarts a stopped machine on that dial) while
// tenants can't reach the gateway's other services or each other.
//
// Lifecycle: the image is pre-baked by the operator —
// nothing is installed at provision time; config changes surface as
// machine-config drift applied on the next EnsureHost. Sleep is owned
// by clank-host's exit keepalive (idle self-exit → machine stops,
// restart policy "no"), wake by Flycast autostart.
package flymachines

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	fly "github.com/superfly/fly-go"
	"github.com/superfly/fly-go/flaps"
	"github.com/superfly/fly-go/tokens"

	"github.com/acksell/clank/pkg/provisioner"
	"github.com/acksell/clank/pkg/provisioner/hoststore"
	transportpkg "github.com/acksell/clank/pkg/provisioner/transport"

	"github.com/oklog/ulid/v2"
)

// Provider is the hoststore provider discriminator. Distinct from
// flysprites (Sprites) so a user's sprite row and machine row
// coexist during migration.
const Provider = "flymachines"

// Provisioner manages one persistent Fly app+machine+volume per
// (userID, "flymachines"). Safe for concurrent use.
type Provisioner struct {
	opts   Options
	log    *log.Logger
	flaps  *flaps.Client
	store  hoststore.HostStore
	preset string

	keyMuMap sync.Mutex
	keyMu    map[string]*sync.Mutex

	cacheMu sync.Mutex
	cache   map[string]*cachedHost
}

type cachedHost struct {
	appName   string
	machineID string
	volumeID  string
	transport http.RoundTripper
	hostID    string
	hostname  string
	url       string
	authToken string
}

// hostTokens bundles the two per-host credentials. auth gates
// INCOMING traffic to clank-host; notifier authenticates OUTGOING
// webhooks back to the gateway. Stable across stop/start — re-read
// from the store row on every resolve.
type hostTokens struct {
	auth     string
	notifier string
}

// New constructs a Provisioner. The HostStore is the persistence
// boundary — laptop daemons pass the SQLite-backed store, cloud
// control planes a Postgres-backed one.
func New(ctx context.Context, opts Options, st hoststore.HostStore, lg *log.Logger) (*Provisioner, error) {
	if st == nil {
		return nil, fmt.Errorf("flymachines: store is required")
	}
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	if lg == nil {
		lg = log.Default()
	}
	fc, err := flaps.NewWithOptions(ctx, flaps.NewClientOpts{
		Tokens:    tokens.Parse(opts.APIToken),
		UserAgent: "clank-flymachines",
	})
	if err != nil {
		return nil, fmt.Errorf("flymachines: construct flaps client: %w", err)
	}
	return &Provisioner{
		opts:   opts,
		log:    lg,
		flaps:  fc,
		store:  st,
		preset: guestPreset(opts),
		keyMu:  map[string]*sync.Mutex{},
		cache:  map[string]*cachedHost{},
	}, nil
}

// GuestPreset exposes the canonical CPU/RAM preset string
// ("shared-8x-4096") so control planes can key usage metering on it.
func (p *Provisioner) GuestPreset() string { return p.preset }

// Stop implements provisioner.Closer. Nothing to release — the flaps
// client is plain HTTP.
func (p *Provisioner) Stop() {}

// EnsureHost implements provisioner.Provisioner.
//
// Detaches from the caller's cancellation (cold create incl. image
// pull runs tens of seconds, longer than request budgets) and bounds
// work with ProvisionTimeout. A per-userID mutex serializes
// concurrent callers onto a single in-flight provision. The warm path
// is an in-process cache hit with zero Fly API calls — the gateway
// calls this on every proxied request.
func (p *Provisioner) EnsureHost(ctx context.Context, userID string) (provisioner.HostRef, error) {
	if userID == "" {
		return provisioner.HostRef{}, fmt.Errorf("flymachines: userID is required")
	}
	mu := p.userMutex(userID)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.opts.ProvisionTimeout)
	defer cancel()

	// Fast path: Flycast autostart wakes a stopped machine on the
	// gateway's dial, so no pre-probe and no API traffic.
	// TODO(ai-review): no TTL/revalidation — out-of-band `fly apps destroy` on a warm-cached user isn't detected until daemon restart (interface contract says EnsureHost detects provider-side deletion). Repo-wide with flysprites. https://github.com/Acksell/clank/pull/128
	if c := p.cacheGet(userID); c != nil {
		return p.refToHost(c), nil
	}

	c, tokens, err := p.resolveOrCreate(ctx, userID)
	if err != nil {
		return provisioner.HostRef{}, err
	}

	// Reconcile the live machine config with the desired one (image
	// bumps, guest resizes, webhook changes). Update restarts the
	// workload, so only drift triggers it.
	if err := p.reconcileMachine(ctx, c, tokens); err != nil {
		return provisioner.HostRef{}, err
	}

	hostPort, err := hostPortOf(c.url)
	if err != nil {
		return provisioner.HostRef{}, fmt.Errorf("machine %s in app %s: %w", c.machineID, c.appName, err)
	}
	transport := &transportpkg.BearerInjector{Token: tokens.auth, Host: hostPort}
	c.transport = transport
	c.authToken = tokens.auth

	// First probe of a stopped machine IS the wake — expect it to take
	// the autostart latency (~1.5s verified) rather than fail.
	if err := waitForHostReady(ctx, c.url, transport); err != nil {
		return provisioner.HostRef{}, fmt.Errorf("machine %s in app %s never reached ready: %w", c.machineID, c.appName, err)
	}

	if err := p.persistRow(ctx, userID, c, tokens); err != nil {
		return provisioner.HostRef{}, fmt.Errorf("persist host row: %w", err)
	}

	p.cacheSet(userID, c)
	return p.refToHost(c), nil
}

func (p *Provisioner) refToHost(c *cachedHost) provisioner.HostRef {
	return provisioner.HostRef{
		HostID:    c.hostID,
		URL:       c.url,
		Transport: c.transport,
		AuthToken: c.authToken,
		AutoWake:  true, // Flycast dial autostarts a stopped machine
		Hostname:  c.hostname,
	}
}

// resolveOrCreate returns the user's app/machine/volume, creating any
// missing piece. The host row is CLAIMED FIRST (claimHostRow) so every
// concurrent instance — in-process or across gateway machines — derives
// identical tokens and machine configs from the same DB state; the
// ensure* walk below is idempotent (get-or-create), so a half-
// provisioned tenant self-heals on the next call, including one whose
// claimer crashed mid-provision. An app deleted out-of-band is simply
// recreated by ensureApp under the row's ExternalID — no row-deleting
// "heal" (a fresh claim's not-yet-created app is indistinguishable from
// a stale one, and deleting the row would race the claim winner).
func (p *Provisioner) resolveOrCreate(ctx context.Context, userID string) (*cachedHost, hostTokens, error) {
	row, err := p.store.GetHostByUser(ctx, userID, Provider)
	if errors.Is(err, hoststore.ErrHostNotFound) {
		row, err = p.claimHostRow(ctx, userID)
	}
	if err != nil {
		return nil, hostTokens{}, fmt.Errorf("resolve host row: %w", err)
	}
	// ExternalID is the source of truth for the actual app name —
	// never re-derive (prefix changes, name-collision suffixes).
	appName := row.ExternalID
	tokens := hostTokens{auth: row.AuthToken, notifier: row.NotifierToken}

	if err := p.ensureApp(ctx, appName); err != nil {
		return nil, hostTokens{}, err
	}
	flycastIP, err := p.ensureFlycast(ctx, appName)
	if err != nil {
		return nil, hostTokens{}, err
	}
	volumeID, err := p.ensureVolume(ctx, row)
	if err != nil {
		return nil, hostTokens{}, err
	}
	machineID, err := p.ensureMachine(ctx, appName, volumeID, tokens, userID)
	if err != nil {
		return nil, hostTokens{}, err
	}

	return &cachedHost{
		appName:   appName,
		machineID: machineID,
		volumeID:  volumeID,
		hostID:    row.ID,
		hostname:  hostnameFor(appName),
		url:       fmt.Sprintf("http://[%s]:%d", flycastIP, HostPort),
	}, tokens, nil
}

// claimHostRow generates candidate tokens and atomically claims the
// (userID, Provider) row; exactly one concurrent claimer wins and
// every other instance reuses the winner's tokens — the fix for the
// 2026-07 incident where two gateway instances each held a DIFFERENT
// in-memory token for 30-60s of cold provision, and the loser's
// "drift" update restarted the machine into a 5-minute 401 window.
//
// The claimed row is a TOKEN CLAIM, not a completion marker: the
// ensure* walk creates any missing infrastructure regardless of row
// state, so a claimer crashing mid-provision is resumed by any later
// caller with no lease/expiry logic. Do not "optimize" this into a
// done-flag.
func (p *Provisioner) claimHostRow(ctx context.Context, userID string) (hoststore.Host, error) {
	auth, err := generateAuthToken()
	if err != nil {
		return hoststore.Host{}, fmt.Errorf("generate auth-token: %w", err)
	}
	notifier, err := generateNotifierToken()
	if err != nil {
		return hoststore.Host{}, fmt.Errorf("generate notifier-token: %w", err)
	}
	appName := appNameFor(p.opts.AppNamePrefix, userID)
	now := time.Now()
	row, created, err := p.store.CreateHostIfAbsent(ctx, hoststore.Host{
		ID:            ulid.Make().String(),
		UserID:        userID,
		Provider:      Provider,
		ExternalID:    appName,
		Hostname:      hostnameFor(appName),
		Status:        hoststore.HostStatusProvisioning,
		AuthToken:     auth,
		NotifierToken: notifier,
		AutoWake:      true,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		return hoststore.Host{}, err
	}
	if !created {
		p.log.Printf("flymachines: host row for user %s already claimed by another instance; reusing its tokens", userID)
	}
	return row, nil
}

// ensureApp creates the per-tenant app on its own private network.
// "already exists" (taken by us) is success; taken by another org is
// surfaced — the operator picks a more distinctive AppNamePrefix.
//
// TODO(ai-review): doesn't verify the existing app belongs to this
// userID before reusing it — appNameFor's 64-bit hash makes a
// same-org collision cryptographically infeasible today, but a real
// fix needs a HostStore lookup-by-ExternalID to fail fast on a
// mismatch. https://github.com/Acksell/clank/pull/128#discussion_r3565338509
func (p *Provisioner) ensureApp(ctx context.Context, appName string) error {
	if _, err := p.flaps.GetApp(ctx, appName); err == nil {
		return nil
	} else if !isNotFound(err) {
		return fmt.Errorf("get app %s: %w", appName, err)
	}
	// TODO(ai-review): a global app-name collision surfaces from GetApp as 401 (not 404), so isNotFound is false and this create path never runs — the "change AppNamePrefix" remedy is unreachable and reads as a broken token. But 401 also = genuinely bad token, so auto-classifying it as name-taken is itself risky; needs the AppNameAvailable check. https://github.com/Acksell/clank/pull/128#discussion_r3565338509
	_, err := p.flaps.CreateApp(ctx, flaps.CreateAppRequest{
		Name:    appName,
		Org:     p.opts.OrgSlug,
		Network: networkNameFor(appName),
	})
	if err != nil {
		// A concurrent instance may have created it between our GetApp
		// and CreateApp — re-check before failing (app create is not
		// idempotent; the name is globally unique).
		if _, getErr := p.flaps.GetApp(ctx, appName); getErr == nil {
			p.log.Printf("flymachines: app %s created concurrently; adopting", appName)
			return nil
		}
		return fmt.Errorf("create app %s (name is global across Fly — if taken, change AppNamePrefix): %w", appName, err)
	}
	p.log.Printf("flymachines: created app %s on network %s", appName, networkNameFor(appName))
	return nil
}

// ensureFlycast allocates the app's private ingress IP on the
// GATEWAY's network (empty GatewayNetwork = the org default network).
// This is the only cross-network object in the design — it's what
// lets the gateway reach an otherwise fully isolated tenant.
func (p *Provisioner) ensureFlycast(ctx context.Context, appName string) (string, error) {
	assignments, err := p.flaps.GetIPAssignments(ctx, appName)
	if err != nil {
		return "", fmt.Errorf("list ip assignments for %s: %w", appName, err)
	}
	// TODO(ai-review): doesn't verify the existing flycast lives on opts.GatewayNetwork — a gateway_network change after tenants exist strands them on an unreachable IP; fix is delete-and-reallocate (fly-go's IPAssignment carries no network field). https://github.com/Acksell/clank/pull/128
	for _, ip := range assignments.IPs {
		if ip.IsFlycast() {
			return ip.IP, nil
		}
	}
	res, err := p.flaps.AssignIP(ctx, appName, flaps.AssignIPRequest{
		Type:         "private_v6",
		Organization: p.opts.OrgSlug,
		Network:      p.opts.GatewayNetwork,
	})
	if err != nil {
		return "", fmt.Errorf("assign flycast for %s: %w", appName, err)
	}
	p.log.Printf("flymachines: allocated flycast %s for app %s", res.IP, appName)
	return res.IP, nil
}

// volumeMetaKey is the ProviderMeta key recording which volume ID this
// host's machine mounts. Volume creation is the one non-idempotent
// ensure step — Fly does NOT enforce unique volume names and IDs are
// server-assigned — so concurrent creators serialize through a
// compare-and-set on this key instead.
const volumeMetaKey = "volume_id"

// ensureVolume resolves the user's single data volume via
// create-then-claim: adopt whatever the row's provider_meta already
// records; otherwise adopt an existing unclaimed volume (a prior
// claimer crashed between create and claim); otherwise create one and
// CAS it — the loser deletes its just-created duplicate (safe by
// construction: a volume is only ever attached AFTER it wins the
// claim) and adopts the winner's. A duplicate whose delete fails is
// reaper food, not a correctness problem. Snapshot retention is
// widened past Fly's 5-day default — daily snapshots are the only
// recovery from a host NVMe loss.
func (p *Provisioner) ensureVolume(ctx context.Context, row hoststore.Host) (string, error) {
	appName := row.ExternalID
	claimed := row.ProviderMeta[volumeMetaKey]
	vols, err := p.flaps.GetVolumes(ctx, appName)
	if err != nil {
		return "", fmt.Errorf("list volumes for %s: %w", appName, err)
	}
	var named []fly.Volume
	for _, v := range vols {
		if v.Name == volumeName {
			named = append(named, v)
		}
	}
	// The claimed volume, while it still exists, is THE volume.
	for _, v := range named {
		if v.ID == claimed {
			return claimed, nil
		}
	}
	if len(named) > 0 {
		// Unclaimed (or stale-claimed — e.g. the app was recreated
		// upstream) volumes exist: adopt the oldest deterministically.
		oldest := named[0]
		for _, v := range named[1:] {
			if v.CreatedAt.Before(oldest.CreatedAt) {
				oldest = v
			}
		}
		won, current, err := p.store.CASProviderMeta(ctx, row.ID, volumeMetaKey, claimed, oldest.ID)
		if err != nil {
			return "", fmt.Errorf("claim adopted volume %s for %s: %w", oldest.ID, appName, err)
		}
		if !won && current == "" {
			return "", fmt.Errorf("volume claim for %s lost with no winner recorded", appName)
		}
		if !won {
			return current, nil
		}
		p.log.Printf("flymachines: adopted volume %s for app %s", oldest.ID, appName)
		return oldest.ID, nil
	}
	// TODO(ai-review): volume region+size are matched by name only, never reconciled — a region change plus a machine re-create requests a cross-region mount Fly rejects forever, and volume_size_gb changes are silently ignored while guest/image DO reconcile. https://github.com/Acksell/clank/pull/128
	size := p.opts.VolumeSizeGB
	retention := DefaultSnapshotRetentionDays
	vol, err := p.flaps.CreateVolume(ctx, appName, fly.CreateVolumeRequest{
		Name:              volumeName,
		Region:            p.opts.Region,
		SizeGb:            &size,
		SnapshotRetention: &retention,
	})
	if err != nil {
		return "", fmt.Errorf("create volume for %s: %w", appName, err)
	}
	won, current, err := p.store.CASProviderMeta(ctx, row.ID, volumeMetaKey, claimed, vol.ID)
	if err != nil {
		// The volume exists but its claim didn't commit — leave it for
		// the next call's adoption pass (or the reaper) rather than
		// guessing here.
		return "", fmt.Errorf("claim created volume %s for %s: %w", vol.ID, appName, err)
	}
	if !won {
		p.log.Printf("flymachines: volume claim lost for app %s; deleting duplicate %s (winner %s)", appName, vol.ID, current)
		if _, delErr := p.flaps.DeleteVolume(ctx, appName, vol.ID); delErr != nil {
			p.log.Printf("flymachines: delete duplicate volume %s: %v (reaper food)", vol.ID, delErr)
		}
		if current == "" {
			return "", fmt.Errorf("volume claim for %s lost with no winner recorded", appName)
		}
		return current, nil
	}
	p.log.Printf("flymachines: created volume %s (%dGB) for app %s", vol.ID, size, appName)
	return vol.ID, nil
}

// ensureMachine launches the user's single machine. ExtraEnvFor is
// applied on this cold-create only (one-shot restore hooks); steady-
// state env stays deterministic for the drift check.
func (p *Provisioner) ensureMachine(ctx context.Context, appName, volumeID string, tokens hostTokens, userID string) (string, error) {
	// ListActive excludes destroyed/destroying machines — a plain List
	// would adopt a dead "clank-host" record and never relaunch.
	machines, err := p.flaps.ListActive(ctx, appName)
	if err != nil {
		return "", fmt.Errorf("list machines for %s: %w", appName, err)
	}
	for _, m := range machines {
		if m.Name == machineName {
			return m.ID, nil
		}
	}
	var oneShot map[string]string
	if p.opts.RestoreURLFor != nil {
		if u := p.opts.RestoreURLFor(userID); u != "" {
			oneShot = map[string]string{restoreEnvKey: u}
		}
	}
	m, err := p.flaps.Launch(ctx, appName, fly.LaunchMachineInput{
		Name:   machineName,
		Region: p.opts.Region,
		Config: buildMachineConfig(p.opts, tokens, volumeID, oneShot),
	})
	if err != nil {
		// A concurrent instance may have launched it between our list
		// and Launch — Fly enforces unique machine names, so the loser
		// sees "already_exists: unique machine name violation" (the
		// 2026-07 incident's literal error). Re-list and adopt instead
		// of surfacing a 500; both instances derive the same config
		// from the claimed row, so whichever launch won is correct.
		machines, listErr := p.flaps.ListActive(ctx, appName)
		if listErr == nil {
			for _, existing := range machines {
				if existing.Name == machineName {
					p.log.Printf("flymachines: machine %s launched concurrently in %s; adopting", existing.ID, appName)
					return existing.ID, nil
				}
			}
		}
		return "", fmt.Errorf("launch machine in %s: %w", appName, err)
	}
	p.log.Printf("flymachines: launched machine %s in app %s", m.ID, appName)
	return m.ID, nil
}

// reconcileMachine applies config drift (image bump, guest resize,
// token/webhook change) via a machine update — but ONLY when the
// machine is stopped. flaps.Update RESTARTS the workload: applied to a
// started machine it kills in-flight agent runs, and in the 2026-07
// provisioning race it restarted a machine mid-provision into a
// 5-minute 401 window. Drift on a started machine is deferred — the
// control plane's out-of-band sweep (ApplyPendingConfig) converges it
// after the next idle-exit, which stops machines within minutes of
// going idle. ExtraEnvFor env (one-shot restore) is preserved from
// the live config: it was cold-create-only by contract, and dropping
// it here would count as perpetual drift.
func (p *Provisioner) reconcileMachine(ctx context.Context, c *cachedHost, tokens hostTokens) error {
	m, err := p.flaps.Get(ctx, c.appName, c.machineID)
	if err != nil {
		return fmt.Errorf("get machine %s: %w", c.machineID, err)
	}
	_, err = p.updateIfStoppedAndDrifted(ctx, c.appName, m, tokens, c.volumeID)
	return err
}

// updateIfStoppedAndDrifted is the single drift-application path shared
// by the EnsureHost cold walk and the ApplyPendingConfig sweep. Returns
// whether an update was applied.
func (p *Provisioner) updateIfStoppedAndDrifted(ctx context.Context, appName string, m *fly.Machine, tokens hostTokens, volumeID string) (bool, error) {
	want := buildMachineConfig(p.opts, tokens, volumeID, oneShotEnv(m.Config))
	if !needsUpdate(m.Config, want) {
		return false, nil
	}
	if m.State != fly.MachineStateStopped {
		p.log.Printf("flymachines: config drift on machine %s in app %s deferred (state %q — never restart an in-use machine; applied when stopped)", m.ID, appName, m.State)
		return false, nil
	}
	p.log.Printf("flymachines: config drift on stopped machine %s in app %s; updating", m.ID, appName)
	if _, err := p.flaps.Update(ctx, appName, fly.LaunchMachineInput{
		ID:     m.ID,
		Config: want,
	}, ""); err != nil {
		return false, fmt.Errorf("update machine %s: %w", m.ID, err)
	}
	return true, nil
}

// ApplyPendingConfig reconciles one user's machine config out-of-band,
// applying drift ONLY to a stopped machine — it never wakes, never
// restarts, so it is always safe to call. The control plane's sweep
// loop drives this (policy — cadence, fleet iteration — lives with the
// caller); idle-exit feeds it naturally since machines stop within
// minutes of going idle. Returns whether an update was applied.
// A user with no host row or no machine is (false, nil): nothing to do.
func (p *Provisioner) ApplyPendingConfig(ctx context.Context, userID string) (bool, error) {
	if userID == "" {
		return false, fmt.Errorf("flymachines: userID is required")
	}
	mu := p.userMutex(userID)
	mu.Lock()
	defer mu.Unlock()

	row, err := p.store.GetHostByUser(ctx, userID, Provider)
	if errors.Is(err, hoststore.ErrHostNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up host for user %s: %w", userID, err)
	}
	machines, err := p.flaps.ListActive(ctx, row.ExternalID)
	if err != nil {
		return false, fmt.Errorf("list machines for %s: %w", row.ExternalID, err)
	}
	for _, m := range machines {
		if m.Name != machineName {
			continue
		}
		// The mounted volume is the live truth; an update must never
		// re-mount, so want-config derives from it (falling back to the
		// row's claim for a machine record without mounts).
		volumeID := row.ProviderMeta[volumeMetaKey]
		if m.Config != nil && len(m.Config.Mounts) == 1 {
			volumeID = m.Config.Mounts[0].Volume
		}
		tokens := hostTokens{auth: row.AuthToken, notifier: row.NotifierToken}
		return p.updateIfStoppedAndDrifted(ctx, row.ExternalID, m, tokens, volumeID)
	}
	return false, nil
}

// oneShotEnv extracts cold-create-only env (CLANK_RESTORE_URL) from a
// live config so reconciliation carries it forward instead of
// treating its presence as drift.
func oneShotEnv(cfg *fly.MachineConfig) map[string]string {
	if cfg == nil {
		return nil
	}
	if v, ok := cfg.Env[restoreEnvKey]; ok {
		return map[string]string{restoreEnvKey: v}
	}
	return nil
}

// waitForHostReady polls GET /status until clank-host answers 200.
// The first request against a stopped machine triggers the Flycast
// autostart, so early connection errors are expected and retried.
// The deadline is the caller's ctx (EnsureHost bounds it with
// ProvisionTimeout) — a cold create's image pull alone can outlast
// any tighter local timer.
// TODO(ai-review): blind /status poll — a crash-looping image burns the full ProvisionTimeout (each probe re-autostarts + re-crashes) and reports only "connection refused"; fly-go's Machine.State + MachineExitEvent (ExitCode/OOMKilled) could fail fast with the real cause. https://github.com/Acksell/clank/pull/128
func waitForHostReady(ctx context.Context, baseURL string, transport http.RoundTripper) error {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	client := &http.Client{Transport: transport}
	statusURL := strings.TrimRight(baseURL, "/") + "/status"
	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, statusURL, nil)
		if err != nil {
			cancel()
			// A malformed statusURL never becomes valid — fail fast
			// instead of nil-deref panicking in client.Do(nil).
			return fmt.Errorf("build status request for %q: %w", statusURL, err)
		}
		resp, err := client.Do(req)
		if err == nil {
			// Read the body BEFORE cancel — cancelling the ctx first
			// aborts the in-flight read, blanking the one diagnostic a
			// non-200 carries.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				cancel()
				return nil
			}
			lastErr = fmt.Errorf("status %d body=%q", resp.StatusCode, snippet(string(body)))
		} else {
			lastErr = err
		}
		cancel()
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for host ready (last: %v)", lastErr)
		case <-tick.C:
		}
	}
}

func snippet(s string) string {
	const max = 120
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// hostPortOf extracts host:port from a URL for bearer pinning. Fails
// fast on a malformed URL (e.g. a garbage flycast IP from the Fly API)
// rather than silently returning the raw string — a mismatched pin
// would drop the bearer, and swallowing it here masks the real fault
// (repo no-fallbacks rule).
func hostPortOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse host URL %q: %w", rawURL, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("host URL %q has no host:port", rawURL)
	}
	return u.Host, nil
}

// persistRow records the ready host's URL + running status on the row
// claimed at the start of resolveOrCreate. The upsert never touches
// provider_meta (CASProviderMeta is its only writer) and the conflict
// clause preserves created_at.
func (p *Provisioner) persistRow(ctx context.Context, userID string, c *cachedHost, tokens hostTokens) error {
	return p.store.UpsertHost(ctx, hoststore.Host{
		ID:            c.hostID,
		UserID:        userID,
		Provider:      Provider,
		ExternalID:    c.appName,
		Hostname:      c.hostname,
		Status:        hoststore.HostStatusRunning,
		LastURL:       c.url,
		AuthToken:     tokens.auth,
		NotifierToken: tokens.notifier,
		AutoWake:      true,
		UpdatedAt:     time.Now(),
	})
}

// SuspendHost stops the machine — a real implementation at last
// (sprites hibernated on their own timer). Idempotent: stopping a
// stopped machine is not an error.
func (p *Provisioner) SuspendHost(ctx context.Context, hostID string) error {
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		return fmt.Errorf("look up host %s: %w", hostID, err)
	}
	// Serialize against a concurrent EnsureHost for the same user so
	// the stop + status write can't interleave with a provision's row
	// write (which would restore stale running-state / an old token).
	mu := p.userMutex(row.UserID)
	mu.Lock()
	defer mu.Unlock()

	machineID, err := p.machineIDForApp(ctx, row.ExternalID, row.UserID)
	if err != nil {
		return err
	}
	if err := p.flaps.Stop(ctx, row.ExternalID, fly.StopMachineInput{ID: machineID}, ""); err != nil && !isAlreadyStopped(err) {
		return fmt.Errorf("stop machine %s in %s: %w", machineID, row.ExternalID, err)
	}
	// Record stopped + drop the cache on every success path, including
	// the already-stopped one (a machine that idle-exited on its own) —
	// otherwise the row keeps reporting Running and the warm cache
	// serves a stale ref.
	row.Status = hoststore.HostStatusStopped
	row.UpdatedAt = time.Now()
	if err := p.store.UpsertHost(ctx, row); err != nil {
		p.log.Printf("flymachines: update status after suspend %s: %v", hostID, err)
	}
	p.cacheDrop(row.UserID)
	return nil
}

// isAlreadyStopped matches Fly's "machine already stopped" response so
// suspending an idle-exited machine is a no-op success. Narrower than
// a bare "already" substring — it must not swallow lease/state errors
// that also contain "already".
func isAlreadyStopped(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already stopped") ||
		strings.Contains(msg, "already in stopped state") ||
		(strings.Contains(msg, "machine still active") && strings.Contains(msg, "stopped"))
}

// machineIDForApp resolves the app's single machine, preferring the
// warm cache.
func (p *Provisioner) machineIDForApp(ctx context.Context, appName, userID string) (string, error) {
	if c := p.cacheGet(userID); c != nil && c.appName == appName {
		return c.machineID, nil
	}
	machines, err := p.flaps.ListActive(ctx, appName)
	if err != nil {
		return "", fmt.Errorf("list machines for %s: %w", appName, err)
	}
	for _, m := range machines {
		if m.Name == machineName {
			return m.ID, nil
		}
	}
	return "", fmt.Errorf("app %s has no %s machine", appName, machineName)
}

// DestroyHost permanently deletes the user's compute: machine, then
// volume EXPLICITLY (don't lean on app-delete cascading — a partial
// failure must never leak a billed volume), then the app (which
// releases the Flycast IP and network), then the store row.
func (p *Provisioner) DestroyHost(ctx context.Context, hostID string) error {
	row, err := p.store.GetHostByID(ctx, hostID)
	if err != nil {
		return fmt.Errorf("look up host %s: %w", hostID, err)
	}
	mu := p.userMutex(row.UserID)
	mu.Lock()
	defer mu.Unlock()
	return p.destroyLocked(ctx, hostID, row.UserID, row.ExternalID)
}

// destroyLocked runs the teardown with the per-user mutex already
// held. Drops the warm cache FIRST (deferred) so any partial upstream
// failure can't leave the EnsureHost fast path serving a half-
// destroyed host — a stale cache entry would brick the user until a
// daemon restart, whereas a dropped cache just re-resolves.
func (p *Provisioner) destroyLocked(ctx context.Context, hostID, userID, appName string) error {
	defer p.cacheDrop(userID)

	machines, err := p.flaps.List(ctx, appName, "")
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("list machines for %s: %w", appName, err)
	}
	for _, m := range machines {
		if err := p.flaps.Destroy(ctx, appName, fly.RemoveMachineInput{ID: m.ID, Kill: true}, ""); err != nil && !isNotFound(err) {
			return fmt.Errorf("destroy machine %s in %s: %w", m.ID, appName, err)
		}
	}
	vols, err := p.flaps.GetVolumes(ctx, appName)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("list volumes for %s: %w", appName, err)
	}
	for _, v := range vols {
		if _, err := p.flaps.DeleteVolume(ctx, appName, v.ID); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete volume %s in %s: %w", v.ID, appName, err)
		}
	}
	if err := p.flaps.DeleteApp(ctx, appName); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete app %s: %w", appName, err)
	}
	if err := p.store.DeleteHostByID(ctx, hostID); err != nil {
		return fmt.Errorf("delete host row %s: %w", hostID, err)
	}
	return nil
}

// DestroyHostsByUser destroys the user's machine host, if any.
// Idempotent; force-destroys regardless of session state (account
// erasure must not be blocked by a busy session). Holds the per-user
// mutex across the lookup AND teardown so a concurrent EnsureHost
// can't re-insert a row after erasure (resurrecting the account).
func (p *Provisioner) DestroyHostsByUser(ctx context.Context, userID string) error {
	mu := p.userMutex(userID)
	mu.Lock()
	defer mu.Unlock()

	row, err := p.store.GetHostByUser(ctx, userID, Provider)
	if errors.Is(err, hoststore.ErrHostNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up host for user %s: %w", userID, err)
	}
	return p.destroyLocked(ctx, row.ID, userID, row.ExternalID)
}

// --- concurrency helpers (same shape as the other providers) ---

// TODO(ai-review): keyMu grows unbounded, one entry per distinct userID ever seen; switch to a bounded/sharded mutex pool repo-wide (flysprites has the same shape) https://github.com/Acksell/clank/pull/128#discussion_r3565295753
func (p *Provisioner) userMutex(userID string) *sync.Mutex {
	p.keyMuMap.Lock()
	defer p.keyMuMap.Unlock()
	if p.keyMu == nil {
		p.keyMu = map[string]*sync.Mutex{}
	}
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
	delete(p.cache, userID)
}

// generateAuthToken returns ~256 bits of URL-safe random.
func generateAuthToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// generateNotifierToken returns a per-host bearer prefixed "clnk_" so
// it's distinguishable from auth-tokens in logs and DB rows.
func generateNotifierToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return "clnk_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// isNotFound reports a genuine flaps 404. It trusts ONLY the typed
// *flaps.FlapsError status — no string fallback: a transport error
// (*url.Error) embeds the request URL, i.e. the app name, whose
// 16-hex-char hash contains "404" for ~1 in 315 users, and misreading
// that as "app gone" triggers destructive recovery (row deletion,
// skipped volume delete). An unclassifiable error is NOT a 404.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if fe, ok := errors.AsType[*flaps.FlapsError](err); ok {
		return fe.ResponseStatusCode == http.StatusNotFound
	}
	return false
}
