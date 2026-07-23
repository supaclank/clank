package acp_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	acpx "github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acp/acptest"
	sdk "github.com/coder/acp-go-sdk"
)

func testProfile(scope acpx.AdapterScope, env func(string) map[string]string) acpx.AdapterProfile {
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

func newSupFixture(t *testing.T, scope acpx.AdapterScope, env func(string) map[string]string) *supFixture {
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
	env := func(string) map[string]string { return map[string]string{"TOKEN": envVal.Load().(string)} }
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

// TestSupervisor_EnvRotatedDuringSpawnTriggersRestart pins the fingerprint
// to the env at spawn *start*. Recording it from a second profileEnv() call
// after spawn returns would race a rotation landing mid-spawn: the recorded
// fingerprint would match the (already-rotated) current env, so reconcile
// would never notice the running process was built from the stale value.
func TestSupervisor_EnvRotatedDuringSpawnTriggersRestart(t *testing.T) {
	t.Parallel()
	var envVal atomic.Value
	envVal.Store("v1")
	env := func(string) map[string]string { return map[string]string{"TOKEN": envVal.Load().(string)} }
	profile := testProfile(acpx.ScopePerDir, env)
	sup, err := acpx.NewAdapterSupervisor(profile, t.Logf)
	if err != nil {
		t.Fatalf("NewAdapterSupervisor: %v", err)
	}
	var spawns atomic.Int64
	inner := acptest.SpawnFunc(func(string) *acptest.ScriptedAgent {
		spawns.Add(1)
		return &acptest.ScriptedAgent{}
	}, profile, t.Logf)
	sup.SetSpawnFunc(func(ctx context.Context, scopeDir string) (*acpx.AdapterProc, error) {
		p, err := inner(ctx, scopeDir)
		if err == nil && spawns.Load() == 1 {
			// Rotation lands while this first spawn is still "in flight"
			// from reconcile's perspective (spawn has just returned but
			// the fingerprint hasn't been recorded yet).
			envVal.Store("v2")
		}
		return p, err
	})
	sup.SetReconcileInterval(20 * time.Millisecond)

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go sup.Run(runCtx)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c1, err := sup.GetConn(ctx, "/dir/a")
	if err != nil {
		t.Fatalf("GetConn: %v", err)
	}

	select {
	case <-c1.Closed():
	case <-time.After(3 * time.Second):
		t.Fatal("rotation mid-spawn went undetected: adapter never restarted onto the rotated env")
	}
	if spawns.Load() != 2 {
		t.Errorf("spawns = %d, want 2 (restart once the rotated env is observed)", spawns.Load())
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

// execSpawn's NewAdapterConn-failure branch (reached after cmd.Start()
// already opened the stdin/stdout pipes) must close them like its sibling
// error branches do — otherwise a supervisor repeatedly failing to spawn
// (bad binary, crashing adapter) leaks 2 fds per attempt until the
// runtime GC finalizes them. Exercises the real execSpawn (no SpawnFunc
// override): /bin/true exits immediately without speaking ACP, so the
// initialize handshake fails right after a successful process start.
func TestSupervisor_ExecSpawnClosesPipesOnInitializeFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/fd is Linux-only")
	}
	// Not t.Parallel(): disabling GC is process-global, and this asserts
	// an exact fd-count delta that concurrent tests' own pipes would pollute.
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)

	profile := testProfile(acpx.ScopePerDir, nil)
	profile.Command = func(string) (string, []string) { return "/bin/true", nil }
	sup, err := acpx.NewAdapterSupervisor(profile, t.Logf)
	if err != nil {
		t.Fatalf("NewAdapterSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("ReadDir /proc/self/fd: %v", err)
	}
	if _, err := sup.GetConn(context.Background(), t.TempDir()); err == nil {
		t.Fatal("GetConn against /bin/true should fail the ACP handshake")
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("ReadDir /proc/self/fd (after): %v", err)
	}
	if got, want := len(after), len(before); got != want {
		t.Errorf("open fds after a failed spawn = %d, want %d (unchanged) — stdin/stdout pipes leaked", got, want)
	}
}
