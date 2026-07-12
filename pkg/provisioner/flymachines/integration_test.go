package flymachines

// Real-Fly integration tests. Skipped unless FLY_API_TOKEN is in the
// env so default `go test ./...` stays fast and cloud-free.
//
//	FLY_API_TOKEN=$(fly tokens create org -o <org>) \
//	FLY_TEST_ORG=<org> FLY_TEST_REGION=arn \
//	go test -count=1 -run TestIntegration_FlyMachines ./pkg/provisioner/flymachines/
//
// Two tiers:
//   - API tier (token only): app/network/flycast/volume/machine
//     provisioning, idempotence, API-normalization drift, destroy.
//   - Network tier (additionally needs a WireGuard peer into the
//     org's default network, e.g. `fly wireguard create` activated):
//     full EnsureHost with readiness probe, suspend → autostart wake.
//     Skipped with instructions when the flycast IP is unreachable.
//
// Uses a fixed test user (deterministic app name) with pre/post
// DeleteApp cleanup — nothing leaks across runs.

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

const integrationUserID = "integration-test"

func integrationOptions(t *testing.T) Options {
	t.Helper()
	token := os.Getenv("FLY_API_TOKEN")
	if token == "" {
		t.Skip("integration test: FLY_API_TOKEN not set — skipping")
	}
	org := os.Getenv("FLY_TEST_ORG")
	if org == "" {
		t.Skip("integration test: FLY_TEST_ORG not set — skipping")
	}
	region := os.Getenv("FLY_TEST_REGION")
	if region == "" {
		region = "arn"
	}
	// Any small amd64 image that answers 200 on :8080 works for the
	// API tier; jmalloc/echo-server answers every path (incl.
	// /status). Override with the real clank-host-fly image (and a
	// WireGuard peer) for the network tier.
	image := os.Getenv("CLANK_TEST_HOST_IMAGE")
	if image == "" {
		image = "jmalloc/echo-server:latest"
	}
	return Options{
		APIToken:      token,
		OrgSlug:       org,
		Region:        region,
		Image:         image,
		AppNamePrefix: "clank-test",
		// Smallest guest — these tests bill real cents.
		GuestCPUKind:  "shared",
		GuestCPUs:     1,
		GuestMemoryMB: 256,
		SwapSizeMB:    64,
		VolumeSizeGB:  1,
	}
}

func newIntegrationProvisioner(t *testing.T) *Provisioner {
	t.Helper()
	opts := integrationOptions(t)
	p, err := New(context.Background(), opts, mustOpenStore(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Pre/post cleanup of the fixed test app: DeleteApp cascades
	// machines, volumes and IPs, so one call resets the world.
	appName := appNameFor(opts.AppNamePrefix, integrationUserID)
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = p.flaps.DeleteApp(ctx, appName)
	}
	cleanup()
	t.Cleanup(cleanup)
	return p
}

func TestIntegration_FlyMachines_ProvisionLifecycle(t *testing.T) {
	p := newIntegrationProvisioner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// --- cold create ---
	c, tokens, err := p.resolveOrCreate(ctx, integrationUserID)
	if err != nil {
		t.Fatalf("resolveOrCreate (cold): %v", err)
	}
	if c.appName == "" || c.machineID == "" || c.volumeID == "" || c.url == "" {
		t.Fatalf("incomplete cachedHost: %+v", c)
	}
	if tokens.auth == "" || tokens.notifier == "" {
		t.Fatal("cold create minted empty tokens")
	}

	app, err := p.flaps.GetApp(ctx, c.appName)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if app.Network != networkNameFor(c.appName) {
		t.Errorf("app network = %q, want per-tenant %q", app.Network, networkNameFor(c.appName))
	}

	// --- idempotence: second resolve must reuse, not duplicate ---
	// (row wasn't persisted yet — resolveOrCreate must still converge
	// on the same upstream objects via get-or-create.)
	c2, _, err := p.resolveOrCreate(ctx, integrationUserID)
	if err != nil {
		t.Fatalf("resolveOrCreate (warm): %v", err)
	}
	if c2.machineID != c.machineID || c2.volumeID != c.volumeID || c2.url != c.url {
		t.Errorf("second resolve diverged: %+v vs %+v", c2, c)
	}
	machines, err := p.flaps.List(ctx, c.appName, "")
	if err != nil {
		t.Fatalf("List machines: %v", err)
	}
	if len(machines) != 1 {
		t.Fatalf("want exactly 1 machine after double-resolve, got %d", len(machines))
	}
	vols, err := p.flaps.GetVolumes(ctx, c.appName)
	if err != nil {
		t.Fatalf("GetVolumes: %v", err)
	}
	if len(vols) != 1 {
		t.Fatalf("want exactly 1 volume after double-resolve, got %d", len(vols))
	}

	// --- API-normalization drift: the round-tripped config must NOT
	// read as drifted, or every EnsureHost would restart the machine.
	// This is the assertion that catches Fly changing response
	// encodings (e.g. autostop bool↔string) under us. ---
	m, err := p.flaps.Get(ctx, c.appName, c.machineID)
	if err != nil {
		t.Fatalf("Get machine: %v", err)
	}
	want := buildMachineConfig(p.opts, tokens, c.volumeID, oneShotEnv(m.Config))
	if needsUpdate(m.Config, want) {
		t.Errorf("freshly-launched machine reads as drifted — API normalization broke the compare.\nhave: %+v\nwant: %+v", m.Config, want)
	}

	// --- drift applies: image change triggers an update. The update
	// is asynchronous on Fly's side, so poll for convergence. ---
	p.opts.Image = "jmalloc/echo-server:v0.3.7"
	if err := p.reconcileMachine(ctx, c, tokens); err != nil {
		t.Fatalf("reconcileMachine (image bump): %v", err)
	}
	updateDeadline := time.Now().Add(60 * time.Second)
	for {
		m, err = p.flaps.Get(ctx, c.appName, c.machineID)
		if err != nil {
			t.Fatalf("Get machine after update: %v", err)
		}
		if m.Config.Image == "jmalloc/echo-server:v0.3.7" {
			break
		}
		if time.Now().After(updateDeadline) {
			t.Fatalf("image after reconcile = %q, never converged to the bumped tag", m.Config.Image)
		}
		time.Sleep(2 * time.Second)
	}

	// --- destroy: everything gone, idempotent ---
	if _, err := p.persistRow(ctx, integrationUserID, c, tokens); err != nil {
		t.Fatalf("persistRow: %v", err)
	}
	if err := p.DestroyHostsByUser(ctx, integrationUserID); err != nil {
		t.Fatalf("DestroyHostsByUser: %v", err)
	}
	if _, err := p.flaps.GetApp(ctx, c.appName); !isNotFound(err) {
		t.Errorf("app still exists after destroy (err=%v)", err)
	}
	if err := p.DestroyHostsByUser(ctx, integrationUserID); err != nil {
		t.Errorf("second DestroyHostsByUser not idempotent: %v", err)
	}
}

// TestIntegration_FlyMachines_EndToEnd exercises the full EnsureHost
// (readiness probe through Flycast) plus suspend → autostart wake.
// Needs a WireGuard peer into the org's default network on top of the
// API-tier env; skips with instructions otherwise.
func TestIntegration_FlyMachines_EndToEnd(t *testing.T) {
	p := newIntegrationProvisioner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ref, err := p.EnsureHost(ctx, integrationUserID)
	if err != nil {
		// Distinguish "no network path" (skip) from a real failure:
		// provisioning succeeded iff the app exists.
		appName := appNameFor(p.opts.AppNamePrefix, integrationUserID)
		if _, appErr := p.flaps.GetApp(ctx, appName); appErr == nil {
			t.Skipf("EnsureHost readiness probe failed (%v) — flycast is only reachable inside the org network; run `fly wireguard create` and activate the peer to run this tier", err)
		}
		t.Fatalf("EnsureHost: %v", err)
	}
	if !ref.AutoWake || ref.URL == "" || ref.Transport == nil {
		t.Fatalf("incomplete HostRef: %+v", ref)
	}

	// Suspend → machine stops; the next dial must wake it (autostart).
	if err := p.SuspendHost(ctx, ref.HostID); err != nil {
		t.Fatalf("SuspendHost: %v", err)
	}
	waitForState := func(state string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			mid, err := p.machineIDForApp(ctx, appNameFor(p.opts.AppNamePrefix, integrationUserID), integrationUserID)
			if err == nil {
				if m, err := p.flaps.Get(ctx, appNameFor(p.opts.AppNamePrefix, integrationUserID), mid); err == nil && m.State == state {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("machine never reached state %q", state)
			}
			time.Sleep(time.Second)
		}
	}
	waitForState("stopped")

	// The wake path: a plain TCP dial of the flycast address.
	start := time.Now()
	conn, err := net.DialTimeout("tcp", hostPortOf(ref.URL), 15*time.Second)
	if err != nil {
		t.Fatalf("dial stopped machine via flycast: %v", err)
	}
	conn.Close()
	t.Logf("autostart wake completed in %s", time.Since(start).Round(time.Millisecond))
	waitForState("started")
}
