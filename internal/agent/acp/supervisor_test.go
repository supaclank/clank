package acp_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	acpx "github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acp/acptest"
	sdk "github.com/coder/acp-go-sdk"
)

func testProfile(scope acpx.AdapterScope, env func() map[string]string) acpx.AdapterProfile {
	return acpx.AdapterProfile{
		ID:      "test-adapter",
		Backend: agent.BackendOpenCode,
		Scope:   scope,
		Command: func(string) (string, []string) { return "unused-in-tests", nil },
		Env:     env,
	}
}

// supFixture runs a supervisor against in-process scripted agents with a
// fast reconcile loop.
type supFixture struct {
	sup      *acpx.AdapterSupervisor
	spawns   atomic.Int64
	lastProc atomic.Pointer[acpx.AdapterProc]
	cancel   context.CancelFunc
}

func newSupFixture(t *testing.T, scope acpx.AdapterScope, env func() map[string]string) *supFixture {
	t.Helper()
	profile := testProfile(scope, env)
	sup, err := acpx.NewAdapterSupervisor(profile, t.Logf)
	if err != nil {
		t.Fatalf("NewAdapterSupervisor: %v", err)
	}
	f := &supFixture{sup: sup}
	inner := acptest.SpawnFunc(func(string) *acptest.ScriptedAgent {
		f.spawns.Add(1)
		return &acptest.ScriptedAgent{}
	}, profile, t.Logf)
	sup.SetSpawnFunc(func(ctx context.Context, scopeDir string) (*acpx.AdapterProc, error) {
		p, err := inner(ctx, scopeDir)
		if err == nil {
			f.lastProc.Store(p)
		}
		return p, err
	})
	sup.SetReconcileInterval(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go sup.Run(ctx)
	t.Cleanup(cancel)
	return f
}

func TestSupervisor_GetConnStartsAndReuses(t *testing.T) {
	t.Parallel()
	f := newSupFixture(t, acpx.ScopePerDir, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c1, err := f.sup.GetConn(ctx, "/dir/a")
	if err != nil {
		t.Fatalf("GetConn a: %v", err)
	}
	if c1.Init().ProtocolVersion != sdk.ProtocolVersionNumber {
		t.Fatalf("initialize not cached: %+v", c1.Init())
	}
	c2, err := f.sup.GetConn(ctx, "/dir/a")
	if err != nil {
		t.Fatalf("GetConn a again: %v", err)
	}
	if c1 != c2 {
		t.Error("same scope dir should reuse the conn")
	}
	if _, err := f.sup.GetConn(ctx, "/dir/b"); err != nil {
		t.Fatalf("GetConn b: %v", err)
	}
	if got := f.spawns.Load(); got != 2 {
		t.Errorf("spawns = %d, want 2 (one per dir)", got)
	}
}

func TestSupervisor_HostScopeSharesOneProcess(t *testing.T) {
	t.Parallel()
	f := newSupFixture(t, acpx.ScopeHost, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c1, err := f.sup.GetConn(ctx, "/dir/a")
	if err != nil {
		t.Fatalf("GetConn: %v", err)
	}
	c2, err := f.sup.GetConn(ctx, "/dir/b")
	if err != nil {
		t.Fatalf("GetConn: %v", err)
	}
	if c1 != c2 || f.spawns.Load() != 1 {
		t.Errorf("host scope: want one shared process, got spawns=%d same=%v", f.spawns.Load(), c1 == c2)
	}
}

func TestSupervisor_RespawnsAfterDeath(t *testing.T) {
	t.Parallel()
	f := newSupFixture(t, acpx.ScopePerDir, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c1, err := f.sup.GetConn(ctx, "/dir/a")
	if err != nil {
		t.Fatalf("GetConn: %v", err)
	}
	// Simulate adapter death: tear down the in-process transport.
	f.lastProc.Load().Stop()
	select {
	case <-c1.Closed():
	case <-time.After(3 * time.Second):
		t.Fatal("conn did not observe transport death")
	}

	// The dir stays desired, so reconcile respawns; GetConn returns the
	// replacement (possibly after a few fast ticks).
	var c2 *acpx.AdapterConn
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c2, err = f.sup.GetConn(ctx, "/dir/a")
		if err == nil && c2 != c1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c2 == nil || c2 == c1 {
		t.Fatalf("no replacement conn after death (err=%v)", err)
	}
	if f.spawns.Load() < 2 {
		t.Errorf("spawns = %d, want >= 2", f.spawns.Load())
	}
}

func TestSupervisor_EnvChangeRestarts(t *testing.T) {
	t.Parallel()
	var envVal atomic.Value
	envVal.Store("v1")
	env := func() map[string]string { return map[string]string{"TOKEN": envVal.Load().(string)} }
	f := newSupFixture(t, acpx.ScopePerDir, env)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c1, err := f.sup.GetConn(ctx, "/dir/a")
	if err != nil {
		t.Fatalf("GetConn: %v", err)
	}
	envVal.Store("v2")
	f.sup.Nudge()

	select {
	case <-c1.Closed():
	case <-time.After(3 * time.Second):
		t.Fatal("old conn not closed after env change")
	}
	c2, err := f.sup.GetConn(ctx, "/dir/a")
	if err != nil {
		t.Fatalf("GetConn after env change: %v", err)
	}
	if c2 == c1 {
		t.Error("env change should produce a fresh conn")
	}
	if f.spawns.Load() != 2 {
		t.Errorf("spawns = %d, want 2", f.spawns.Load())
	}
}

func TestSupervisor_SpawnErrorFailsWaiterThenRecovers(t *testing.T) {
	t.Parallel()
	profile := testProfile(acpx.ScopePerDir, nil)
	sup, err := acpx.NewAdapterSupervisor(profile, t.Logf)
	if err != nil {
		t.Fatalf("NewAdapterSupervisor: %v", err)
	}
	var attempts atomic.Int64
	good := acptest.SpawnFunc(func(string) *acptest.ScriptedAgent { return &acptest.ScriptedAgent{} }, profile, t.Logf)
	sup.SetSpawnFunc(func(ctx context.Context, scopeDir string) (*acpx.AdapterProc, error) {
		if attempts.Add(1) == 1 {
			return nil, fmt.Errorf("boom")
		}
		return good(ctx, scopeDir)
	})
	sup.SetReconcileInterval(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer callCancel()
	if _, err := sup.GetConn(callCtx, "/dir/a"); err == nil {
		t.Fatal("first GetConn should surface the spawn error")
	}
	// The dir stays desired; a later call succeeds once spawn recovers.
	if _, err := sup.GetConn(callCtx, "/dir/a"); err != nil {
		t.Fatalf("second GetConn should recover: %v", err)
	}
}

func TestSupervisor_StopAllFailsWaitersAndConns(t *testing.T) {
	t.Parallel()
	f := newSupFixture(t, acpx.ScopePerDir, nil)
	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	c1, err := f.sup.GetConn(ctx, "/dir/a")
	if err != nil {
		t.Fatalf("GetConn: %v", err)
	}
	f.cancel() // ctx cancel → Run exits → StopAll

	select {
	case <-c1.Closed():
	case <-time.After(3 * time.Second):
		t.Fatal("conn not closed by StopAll")
	}
	if _, err := f.sup.GetConn(context.Background(), "/dir/a"); err == nil {
		t.Error("GetConn after StopAll should error")
	}
}
