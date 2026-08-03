package flymachines

// Regression tests for the 2026-07-14 multi-instance provisioning race
// (clankgw-dev, 2 gateway machines): each instance minted a DIFFERENT
// auth token in memory (persistRow only ran after waitForHostReady),
// the loser hit "already_exists: unique machine name violation" on
// launch, then flagged spurious config drift, rewrote the machine's
// CLANK_HOST_AUTH_TOKEN via flaps.Update and RESTARTED it — the other
// instance's readiness poll then got 401 for the full ProvisionTimeout
// (user-visible: first provision hung 5-10 minutes).
//
// These tests run TWO real Provisioner instances against ONE real
// SQLite store and ONE fake Fly API (real HTTP through the real flaps
// client). Not t.Parallel: FLY_FLAPS_BASE_URL is process-global env,
// read by flaps.NewWithOptions at construction (t.Setenv forbids
// Parallel).

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"

	"github.com/supaclank/clank/internal/store"
	"github.com/supaclank/clank/pkg/provisioner/hoststore"
)

// newFakeFlyProvisioner builds a Provisioner whose flaps client talks
// to the given fakeFly. Call t.Setenv(FLY_FLAPS_BASE_URL) BEFORE this.
func newFakeFlyProvisioner(t *testing.T, s *store.Store) *Provisioner {
	t.Helper()
	p, err := New(context.Background(), Options{
		APIToken: "fake-token",
		OrgSlug:  "fake-org",
		Region:   "arn",
		Image:    "registry.example/clank-host:v1",
	}, s, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// TestConcurrentProvision_ConvergesWithoutSpuriousUpdate is the direct
// regression test for the incident: two instances resolving the same
// user concurrently must converge on ONE auth token, ONE volume, ONE
// machine — and issue ZERO machine updates (the spurious "drift"
// restart is what turned the race into a 5-minute 401 outage).
func TestConcurrentProvision_ConvergesWithoutSpuriousUpdate(t *testing.T) {
	ff := newFakeFly(t)
	t.Setenv("FLY_FLAPS_BASE_URL", ff.srv.URL)
	s := mustOpenStore(t)
	p1 := newFakeFlyProvisioner(t, s)
	p2 := newFakeFlyProvisioner(t, s)
	ctx := context.Background()

	type result struct {
		c      *cachedHost
		tokens hostTokens
		err    error
	}
	var wg sync.WaitGroup
	results := make([]result, 2)
	for i, p := range []*Provisioner{p1, p2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, tokens, err := p.resolveOrCreate(ctx, "user-race")
			results[i] = result{c, tokens, err}
		}()
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("instance %d resolveOrCreate: %v", i, r.err)
		}
	}
	if results[0].tokens != results[1].tokens {
		t.Errorf("token divergence across instances:\n  p1=%+v\n  p2=%+v", results[0].tokens, results[1].tokens)
	}
	if results[0].c.volumeID != results[1].c.volumeID {
		t.Errorf("volume divergence: p1=%s p2=%s", results[0].c.volumeID, results[1].c.volumeID)
	}
	if results[0].c.machineID != results[1].c.machineID {
		t.Errorf("machine divergence: p1=%s p2=%s", results[0].c.machineID, results[1].c.machineID)
	}
	if results[0].c.hostID != results[1].c.hostID {
		t.Errorf("host row divergence: p1=%s p2=%s", results[0].c.hostID, results[1].c.hostID)
	}

	appName := results[0].c.appName
	vCreates, vDeletes, launches, updates := ff.snapshot()
	if updates != 0 {
		t.Errorf("machine updates during concurrent provision = %d, want 0 (an update RESTARTS the machine — the incident's outage amplifier)", updates)
	}
	if launches != 1 {
		t.Fatalf("successful machine launches = %d, want exactly 1", launches)
	}
	if got := ff.volumeCount(appName); got != 1 {
		t.Errorf("volumes on app after race = %d (creates=%d deletes=%d), want exactly 1", got, vCreates, vDeletes)
	}

	// The machine's baked env must carry the CONVERGED token — a machine
	// running a token no row holds is the 401 brick.
	row, err := s.GetHostByUser(ctx, "user-race", Provider)
	if err != nil {
		t.Fatalf("GetHostByUser: %v", err)
	}
	if row.AuthToken != results[0].tokens.auth {
		t.Errorf("row token %q != resolved token %q", row.AuthToken, results[0].tokens.auth)
	}
	ff.mu.Lock()
	m := ff.apps[appName].machines[0]
	ff.mu.Unlock()
	if got := m.Config.Env["CLANK_HOST_AUTH_TOKEN"]; got != row.AuthToken {
		t.Errorf("machine env token %q != row token %q — the machine would 401 every gateway probe", got, row.AuthToken)
	}
	if got := row.ProviderMeta[volumeMetaKey]; got != results[0].c.volumeID {
		t.Errorf("provider_meta volume claim %q != resolved volume %q", got, results[0].c.volumeID)
	}
}

// TestEnsureVolume_AdoptsUnclaimedVolume: a claimer that crashed
// between CreateVolume and its CAS leaves an unclaimed volume — the
// next caller must adopt it instead of creating a duplicate.
func TestEnsureVolume_AdoptsUnclaimedVolume(t *testing.T) {
	ff := newFakeFly(t)
	t.Setenv("FLY_FLAPS_BASE_URL", ff.srv.URL)
	s := mustOpenStore(t)
	p := newFakeFlyProvisioner(t, s)
	ctx := context.Background()

	claim, err := p.claimHostRow(ctx, "user-adopt")
	if err != nil {
		t.Fatalf("claimHostRow: %v", err)
	}
	ff.addVolume(claim.ExternalID, "vol_orphaned", volumeName)

	got, err := p.ensureVolume(ctx, claim)
	if err != nil {
		t.Fatalf("ensureVolume: %v", err)
	}
	if got != "vol_orphaned" {
		t.Errorf("ensureVolume = %q, want adopted vol_orphaned", got)
	}
	vCreates, _, _, _ := ff.snapshot()
	if vCreates != 0 {
		t.Errorf("volume creates = %d, want 0 (adoption, not duplication)", vCreates)
	}
	row, err := s.GetHostByID(ctx, claim.ID)
	if err != nil {
		t.Fatalf("GetHostByID: %v", err)
	}
	if row.ProviderMeta[volumeMetaKey] != "vol_orphaned" {
		t.Errorf("claim not persisted: provider_meta=%v", row.ProviderMeta)
	}
}

// TestEnsureVolume_LoserDeletesDuplicate: the create-then-claim loser
// path — another instance claimed a volume between our stale row read
// and our create, so we must delete our just-created duplicate and
// adopt the winner's.
func TestEnsureVolume_LoserDeletesDuplicate(t *testing.T) {
	ff := newFakeFly(t)
	t.Setenv("FLY_FLAPS_BASE_URL", ff.srv.URL)
	s := mustOpenStore(t)
	p := newFakeFlyProvisioner(t, s)
	ctx := context.Background()

	claim, err := p.claimHostRow(ctx, "user-loser")
	if err != nil {
		t.Fatalf("claimHostRow: %v", err)
	}
	// The app must exist for volume calls; ensureApp is idempotent.
	if err := p.ensureApp(ctx, claim.ExternalID); err != nil {
		t.Fatalf("ensureApp: %v", err)
	}
	// Simulate the winner: DB meta already claims vol_winner, but OUR
	// row snapshot (claim) still has empty meta — the stale-read race.
	if won, _, err := s.CASProviderMeta(ctx, claim.ID, volumeMetaKey, "", "vol_winner"); err != nil || !won {
		t.Fatalf("seed winner claim: won=%v err=%v", won, err)
	}

	got, err := p.ensureVolume(ctx, claim)
	if err != nil {
		t.Fatalf("ensureVolume: %v", err)
	}
	if got != "vol_winner" {
		t.Errorf("ensureVolume = %q, want the winner's vol_winner", got)
	}
	vCreates, vDeletes, _, _ := ff.snapshot()
	if vCreates != 1 || vDeletes != 1 {
		t.Errorf("creates=%d deletes=%d, want 1/1 (loser creates then deletes its duplicate)", vCreates, vDeletes)
	}
	if ff.volumeCount(claim.ExternalID) != 0 {
		t.Errorf("duplicate volume left behind on the app")
	}
}

// TestReconcile_NeverUpdatesStartedMachine pins the gate that turns
// config rollouts from workload restarts into deferred, stopped-only
// updates — and that ApplyPendingConfig converges a stopped machine.
func TestReconcile_NeverUpdatesStartedMachine(t *testing.T) {
	ff := newFakeFly(t)
	t.Setenv("FLY_FLAPS_BASE_URL", ff.srv.URL)
	s := mustOpenStore(t)
	p := newFakeFlyProvisioner(t, s)
	ctx := context.Background()

	c, tokens, err := p.resolveOrCreate(ctx, "user-drift")
	if err != nil {
		t.Fatalf("resolveOrCreate: %v", err)
	}
	// Freshly-launched machine must NOT read as drifted.
	if err := p.reconcileMachine(ctx, c, tokens); err != nil {
		t.Fatalf("reconcileMachine (fresh): %v", err)
	}
	if _, _, _, updates := ff.snapshot(); updates != 0 {
		t.Fatalf("fresh machine triggered %d updates, want 0", updates)
	}

	// Real drift (image bump) against a STARTED machine: defer.
	p.opts.Image = "registry.example/clank-host:v2"
	if err := p.reconcileMachine(ctx, c, tokens); err != nil {
		t.Fatalf("reconcileMachine (drift, started): %v", err)
	}
	if _, _, _, updates := ff.snapshot(); updates != 0 {
		t.Fatalf("drifted STARTED machine triggered %d updates, want 0 (update restarts the workload)", updates)
	}
	if applied, err := p.ApplyPendingConfig(ctx, "user-drift"); err != nil || applied {
		t.Fatalf("ApplyPendingConfig (started) = (%v, %v), want (false, nil)", applied, err)
	}

	// Machine stops (idle self-exit) → the sweep applies the update.
	ff.setMachineState(t, c.appName, c.machineID, "stopped")
	applied, err := p.ApplyPendingConfig(ctx, "user-drift")
	if err != nil {
		t.Fatalf("ApplyPendingConfig (stopped): %v", err)
	}
	if !applied {
		t.Fatal("ApplyPendingConfig did not apply to a stopped drifted machine")
	}
	if _, _, _, updates := ff.snapshot(); updates != 1 {
		t.Fatalf("updates = %d, want exactly 1", updates)
	}
	// Converged: a second sweep is a no-op.
	if applied, err := p.ApplyPendingConfig(ctx, "user-drift"); err != nil || applied {
		t.Fatalf("second ApplyPendingConfig = (%v, %v), want (false, nil)", applied, err)
	}
	// And the user with no host row is a clean no-op.
	if applied, err := p.ApplyPendingConfig(ctx, "user-without-host"); err != nil || applied {
		t.Fatalf("ApplyPendingConfig (no row) = (%v, %v), want (false, nil)", applied, err)
	}
}

// TestConcurrentClaim_SingleWinner pins the claim primitive end to end
// through the provisioner: N concurrent claims mint exactly one row.
func TestConcurrentClaim_SingleWinner(t *testing.T) {
	t.Parallel() // store-only: no env, no fake fly
	s := mustOpenStore(t)
	p := &Provisioner{store: s, log: log.New(io.Discard, "", 0)}
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	rows := make([]hoststore.Host, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows[i], errs[i] = p.claimHostRow(ctx, "user-claim")
		}()
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("claim %d: %v", i, errs[i])
		}
		if rows[i].ID != rows[0].ID || rows[i].AuthToken != rows[0].AuthToken {
			t.Fatalf("claim %d diverged: %+v vs %+v", i, rows[i], rows[0])
		}
	}
}
