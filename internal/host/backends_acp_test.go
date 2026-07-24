package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	acpx "github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acp/acptest"
	"github.com/acksell/clank/internal/host"
	sdk "github.com/coder/acp-go-sdk"
)

// End-to-end through the manager: supervisor spawn (scripted agent over
// real pipes), fresh CreateBackend → Open → Send roundtrip, and
// discovery mapping incl. the ghost-row filter.
func TestACPBackendManager_CreateOpenSendAndDiscover(t *testing.T) {
	// installGuidanceSkills writes under $HOME — isolate it.
	t.Setenv("HOME", t.TempDir())

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.PromptFn = func(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
			_ = a.Conn().SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: sdk.UpdateAgentMessageText("ok")})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		}
		a.ListSessionsFn = func(ctx context.Context, p sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
			return sdk.ListSessionsResponse{Sessions: []sdk.SessionInfo{
				{SessionId: "thr-1", Cwd: "/proj/a", Title: sdk.Ptr("Fix the bug"), UpdatedAt: sdk.Ptr(time.Now().Format(time.RFC3339Nano))},
				{SessionId: "thr-ghost", Cwd: "/proj/a"}, // no title, no timestamp → filtered
			}}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("unused-bun", "unused-entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	b, err := mgr.CreateBackend(ctx, agent.BackendInvocation{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}
	defer func() { _ = b.Stop() }()
	callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
	defer callCancel()
	if err := b.OpenAndSend(callCtx, agent.SendMessageOpts{Text: "hi"}); err != nil {
		t.Fatalf("OpenAndSend: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for b.Status() != agent.StatusIdle && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	msgs, err := b.Messages(callCtx)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 || msgs[1].Content != "ok" {
		t.Fatalf("Messages = %+v, want user+assistant with 'ok'", msgs)
	}

	snaps, err := mgr.DiscoverAllSessions(callCtx)
	if err != nil {
		t.Fatalf("DiscoverAllSessions: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d, want 1 (ghost row filtered)", len(snaps))
	}
	s := snaps[0]
	if s.ID != "thr-1" || s.Backend != agent.BackendCodex || s.Title != "Fix the bug" || s.Directory != "/proj/a" {
		t.Errorf("snapshot = %+v", s)
	}
}

// The manager MUST implement agent.ModelLister: Service.ListModels type-
// asserts it and silently returns nil otherwise, which is exactly how
// every backend shipped with an empty model picker.
func TestACPBackendManager_ImplementsModelLister(t *testing.T) {
	t.Parallel()
	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	if _, ok := any(mgr).(agent.ModelLister); !ok {
		t.Fatal("ACPBackendManager does not implement agent.ModelLister — /models will return nil for every ACP backend")
	}
}

// A session's advertised models must reach ListModels for its project dir.
func TestACPBackendManager_ListModelsServesSessionCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	category := sdk.SessionConfigOptionCategory("model")
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			return sdk.NewSessionResponse{
				SessionId: "s-1",
				ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
					Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
					Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
						{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
					}},
				}}},
			}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	// (A cold dir is no longer empty — ListModels probes to fill it; see
	// TestACPBackendManager_ProbesOncePerDirToFillCatalog. This test covers
	// the other direction: a REAL session's catalog reaching ListModels.)
	workDir := t.TempDir()

	b, err := mgr.CreateBackend(ctx, agent.BackendInvocation{WorkDir: workDir})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}
	defer func() { _ = b.Stop() }()
	openCtx, openCancel := context.WithTimeout(ctx, 10*time.Second)
	defer openCancel()
	if err := b.Open(openCtx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	models, err := mgr.ListModels(ctx, workDir)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5.2-codex" || models[0].Name != "GPT-5.2 Codex" {
		t.Fatalf("ListModels after session open = %+v, want the agent-advertised catalog", models)
	}
}

// A cold project dir has no catalog, so the picker would be empty before
// the user's first session. ACP only advertises modes/models on
// session/new, so the manager opens one short-lived session per dir —
// ONCE, and single-flighted, since /modes and /models both miss together.
func TestACPBackendManager_ProbesOncePerDirToFillCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr := newPerDirManager(t, acpDirs(t))
	category := sdk.SessionConfigOptionCategory("model")
	modeCategory := sdk.SessionConfigOptionCategory("mode")
	var sessions atomic.Int64
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			sessions.Add(1)
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("probe-" + strconv.FormatInt(sessions.Load(), 10)),
				ConfigOptions: []sdk.SessionConfigOption{
					{Select: &sdk.SessionConfigOptionSelect{
						Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
						Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
							{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
						}},
					}},
					{Select: &sdk.SessionConfigOptionSelect{
						Id: "mode", Name: "Mode", Category: &modeCategory, CurrentValue: "agent",
						Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
							{Value: "read-only", Name: "Read Only"},
							{Value: "agent", Name: "Agent"},
						}},
					}},
				},
			}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	workDir := t.TempDir()

	// Concurrent cold callers (the TUI fires /modes and /models together).
	// Reads return immediately and single-flight ONE background probe.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = mgr.ListModes(ctx, workDir) }()
	go func() { defer wg.Done(); _, _ = mgr.ListModels(ctx, workDir) }()
	wg.Wait()

	// The probe fills the catalog asynchronously; the picker refines once
	// it lands (the TUI re-fetches).
	waitFor(t, "probe to fill the catalog", func() bool {
		m, _ := mgr.ListModels(ctx, workDir)
		return len(m) == 1
	})
	modes, _ := mgr.ListModes(ctx, workDir)
	models, _ := mgr.ListModels(ctx, workDir)
	if len(modes) != 2 || modes[0].ID != "read-only" {
		t.Errorf("probed modes = %+v, want the agent's advertised list", modes)
	}
	if len(models) != 1 || models[0].ID != "gpt-5.2-codex" {
		t.Errorf("probed models = %+v, want the agent's advertised list", models)
	}
	if got := sessions.Load(); got != 1 {
		t.Fatalf("probe opened %d sessions, want 1 (single-flighted across both callers)", got)
	}

	// Warm dir: served from the catalog, no further sessions.
	_, _ = mgr.ListModes(ctx, workDir)
	_, _ = mgr.ListModels(ctx, workDir)
	if got := sessions.Load(); got != 1 {
		t.Errorf("warm reads opened %d sessions, want no new ones", got)
	}
}

// A transient probe failure (adapter not ready, timeout) must not
// permanently brick the picker: only a successful probe may mark a dir
// as probed, so the next caller gets to retry.
func TestACPBackendManager_ProbeFailureRetriesInsteadOfPermanentlyEmptyingCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr := newPerDirManager(t, acpDirs(t))
	category := sdk.SessionConfigOptionCategory("model")
	var attempts atomic.Int64
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			if attempts.Add(1) == 1 {
				return sdk.NewSessionResponse{}, errors.New("adapter not ready yet")
			}
			return sdk.NewSessionResponse{
				SessionId: "probe-2",
				ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
					Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
					Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
						{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
					}},
				}}},
			}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	workDir := t.TempDir()

	// First read serves the (empty) global catalog and kicks a probe that
	// fails — the dir must NOT be marked probed, or it's stuck empty for the
	// daemon's lifetime. Subsequent reads retry until one probe succeeds.
	first, err := mgr.ListModels(ctx, workDir)
	if err != nil {
		t.Fatalf("ListModels (failed probe): %v", err)
	}
	if len(first) != 0 {
		t.Fatalf("ListModels before any successful probe = %+v, want empty", first)
	}

	waitFor(t, "catalog to fill after a retried probe", func() bool {
		m, _ := mgr.ListModels(ctx, workDir)
		return len(m) == 1
	})
	if got := attempts.Load(); got != 2 {
		t.Fatalf("probe attempts = %d, want 2 (one failure + one retry)", got)
	}

	// Warm dir now: served from the catalog, no further probe attempts.
	_, _ = mgr.ListModels(ctx, workDir)
	if got := attempts.Load(); got != 2 {
		t.Errorf("warm read after success made %d attempts, want no new ones", got)
	}
}

// waitFor polls fn until it returns true or the deadline passes. Background
// probes fill the catalog asynchronously, so tests observe them by re-reading.
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// acpDirs gives a manager isolated on-disk state: a throwaway tools dir
// and a throwaway catalog, so a test never reads or writes the
// developer's real ~/.clank catalog.
func acpDirs(t *testing.T) host.ACPDirs {
	t.Helper()
	return host.ACPDirs{Tools: t.TempDir(), Catalog: t.TempDir()}
}

// newPerDirManager builds a manager whose catalog varies per dir (opencode's
// shape), so ListModels/ListModes trigger a folder probe. It reuses the
// codex scripted profile but flips the scope; host-scoped backends rely on
// Prewarm instead and never folder-probe.
func newPerDirManager(t *testing.T, dirs host.ACPDirs) *host.ACPBackendManager {
	t.Helper()
	profile := acpx.CodexProfile("bun", "entry", nil)
	profile.Scope = acpx.ScopePerDir
	mgr, err := host.NewACPBackendManager(profile, dirs)
	if err != nil {
		t.Fatalf("NewACPBackendManager: %v", err)
	}
	return mgr
}

// A restart (or just revisiting a folder) must not re-probe: the catalog
// a session advertised is persisted per project dir, so a brand-new
// manager over the same catalog dir answers /modes and /models
// immediately, opening no session at all.
func TestACPBackendManager_CatalogSurvivesRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	catalogDir := t.TempDir()
	workDir := t.TempDir()
	var sessions atomic.Int64
	category := sdk.SessionConfigOptionCategory("model")
	modeCategory := sdk.SessionConfigOptionCategory("mode")
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			sessions.Add(1)
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("s-" + strconv.FormatInt(sessions.Load(), 10)),
				ConfigOptions: []sdk.SessionConfigOption{
					{Select: &sdk.SessionConfigOptionSelect{
						Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
						Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
							{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
						}},
					}},
					{Select: &sdk.SessionConfigOptionSelect{
						Id: "mode", Name: "Mode", Category: &modeCategory, CurrentValue: "agent",
						Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
							{Value: "read-only", Name: "Read Only"},
							{Value: "agent", Name: "Agent"},
						}},
					}},
				},
			}, nil
		}
		return a
	}

	// start builds a manager sharing catalogDir — the second call stands in
	// for a daemon restart.
	start := func(t *testing.T) (*host.ACPBackendManager, context.CancelFunc) {
		t.Helper()
		mgr := newPerDirManager(t, host.ACPDirs{Tools: t.TempDir(), Catalog: catalogDir})
		mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
		mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
			cancel()
			t.Fatalf("Init: %v", err)
		}
		return mgr, func() { mgr.Shutdown(); cancel() }
	}

	first, stopFirst := start(t)
	ctx := context.Background()
	waitFor(t, "first probe to fill the catalog", func() bool {
		m, _ := first.ListModels(ctx, workDir)
		return len(m) == 1
	})
	if got := sessions.Load(); got != 1 {
		t.Fatalf("cold dir opened %d sessions, want 1 probe", got)
	}
	stopFirst()

	second, stopSecond := start(t)
	defer stopSecond()

	models, err := second.ListModels(ctx, workDir)
	if err != nil {
		t.Fatalf("ListModels after restart: %v", err)
	}
	modes, err := second.ListModes(ctx, workDir)
	if err != nil {
		t.Fatalf("ListModes after restart: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5.2-codex" {
		t.Errorf("models after restart = %+v, want the persisted catalog", models)
	}
	if len(modes) != 2 || modes[0].ID != "read-only" {
		t.Errorf("modes after restart = %+v, want the persisted catalog", modes)
	}
	if got := sessions.Load(); got != 1 {
		t.Errorf("restart opened %d sessions total, want no new probe", got)
	}
}

// Prewarm fills the backend-global catalog so every dir answers instantly.
// A host-scoped backend (codex, claude) has no per-dir variance, so one
// neutral prewarm serves all dirs and reads open no per-dir session — the
// zero-spinner path for the picker.
func TestACPBackendManager_PrewarmServesEveryDirWithoutPerDirSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	category := sdk.SessionConfigOptionCategory("model")
	modeCategory := sdk.SessionConfigOptionCategory("mode")
	var sessions atomic.Int64
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			sessions.Add(1)
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("s-" + strconv.FormatInt(sessions.Load(), 10)),
				ConfigOptions: []sdk.SessionConfigOption{
					{Select: &sdk.SessionConfigOptionSelect{
						Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
						Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
							{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
						}},
					}},
					{Select: &sdk.SessionConfigOptionSelect{
						Id: "mode", Name: "Mode", Category: &modeCategory, CurrentValue: "agent",
						Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
							{Value: "read-only", Name: "Read Only"},
							{Value: "agent", Name: "Agent"},
						}},
					}},
				},
			}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	mgr.Prewarm(ctx) // synchronous when called directly
	if got := sessions.Load(); got != 1 {
		t.Fatalf("prewarm opened %d sessions, want 1 neutral probe", got)
	}

	// Any dir now serves the global catalog and opens no further sessions.
	for _, d := range []string{"/proj/a", "/proj/b", t.TempDir()} {
		models, _ := mgr.ListModels(ctx, d)
		modes, _ := mgr.ListModes(ctx, d)
		if len(models) != 1 || models[0].ID != "gpt-5.2-codex" {
			t.Errorf("models for %s = %+v, want the global catalog", d, models)
		}
		if len(modes) != 2 || modes[0].ID != "read-only" {
			t.Errorf("modes for %s = %+v, want the global catalog", d, modes)
		}
	}
	if got := sessions.Load(); got != 1 {
		t.Errorf("reads after prewarm opened %d sessions, want no new ones", got)
	}
}

// Prewarm's per-dir sweep must single-flight against a concurrent live read
// for the same dir, not just against itself: racing a ListModes call while
// Prewarm's own probe for that dir is still in flight must not open a
// second session.
func TestACPBackendManager_PrewarmSingleFlightsAgainstConcurrentLiveRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	workDir := t.TempDir()
	mgr := newPerDirManager(t, acpDirs(t))
	category := sdk.SessionConfigOptionCategory("model")
	var sessions, workDirSessions atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	// Prewarm's neutral probe (a separate scopeDir) must open and complete
	// unblocked — only the workDir-scoped probe holds, so `started`/`release`
	// isolate the per-dir race instead of the unrelated neutral one.
	scripted := func(scopeDir string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			n := sessions.Add(1)
			if scopeDir == workDir && workDirSessions.Add(1) == 1 {
				close(started)
				<-release
			}
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("s-" + strconv.FormatInt(n, 10)),
				ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
					Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
					Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
						{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
					}},
				}}},
			}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return []string{workDir}, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); mgr.Prewarm(ctx) }()

	<-started                          // Prewarm's per-dir probe for workDir is now in flight
	_, _ = mgr.ListModes(ctx, workDir) // must not open a second session for the same dir
	close(release)
	wg.Wait()

	waitFor(t, "probe to fill the catalog", func() bool {
		m, _ := mgr.ListModels(ctx, workDir)
		return len(m) == 1
	})
	if got := workDirSessions.Load(); got != 1 {
		t.Fatalf("prewarm + concurrent live read opened %d sessions for workDir, want 1 (single-flighted)", got)
	}
}

// A host-scoped backend has no per-dir probe, so if the startup Prewarm
// never filled the global catalog (adapter still provisioning), a read must
// still recover: it serves empty immediately and re-probes the global
// catalog in the background — once.
func TestACPBackendManager_HostScopeRecoversGlobalCatalogWithoutPrewarm(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	category := sdk.SessionConfigOptionCategory("model")
	var sessions atomic.Int64
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			sessions.Add(1)
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("s-" + strconv.FormatInt(sessions.Load(), 10)),
				ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
					Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
					Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
						{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
					}},
				}}},
			}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	// No Prewarm. The first read is empty but kicks a background global probe.
	if m, _ := mgr.ListModels(ctx, t.TempDir()); len(m) != 0 {
		t.Fatalf("first read = %+v, want empty (recovery probe is async)", m)
	}
	waitFor(t, "lazy global probe to fill the catalog", func() bool {
		m, _ := mgr.ListModels(ctx, t.TempDir())
		return len(m) == 1
	})
	if got := sessions.Load(); got != 1 {
		t.Errorf("recovery opened %d sessions, want 1 (single-flighted)", got)
	}
}

// A neutral/global probe opens its session in a throwaway temp dir that's
// removed the moment the probe returns. The per-session catalog sinks fire
// during Open regardless of who called it, so without a guard they persist
// a dead entry keyed by that already-deleted path — accumulating one per
// Prewarm/recovery call, forever, across every restart.
func TestACPBackendManager_GlobalProbeDoesNotPersistNeutralDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	catalogDir := t.TempDir()
	mgr, err := host.NewCodexACPManager(host.ACPDirs{Tools: t.TempDir(), Catalog: catalogDir})
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	category := sdk.SessionConfigOptionCategory("model")
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			return sdk.NewSessionResponse{
				SessionId: sdk.SessionId("s-1"),
				ConfigOptions: []sdk.SessionConfigOption{{Select: &sdk.SessionConfigOptionSelect{
					Id: "model", Name: "Model", Category: &category, CurrentValue: "gpt-5.2-codex",
					Options: sdk.SessionConfigSelectOptions{Ungrouped: &sdk.SessionConfigSelectOptionsUngrouped{
						{Value: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
					}},
				}}},
			}, nil
		}
		return a
	}
	mgr.Supervisor().SetSpawnFunc(acptest.SpawnFunc(scripted, acpx.CodexProfile("bun", "entry", nil), t.Logf))
	mgr.Supervisor().SetReconcileInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer mgr.Shutdown()

	mgr.Prewarm(ctx) // synchronous when called directly; opens the neutral probe

	raw, err := os.ReadFile(filepath.Join(catalogDir, "codex.json"))
	if err != nil {
		t.Fatalf("read catalog file: %v", err)
	}
	var file struct {
		Dirs map[string]json.RawMessage `json:"dirs"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse catalog file: %v", err)
	}
	if len(file.Dirs) != 0 {
		t.Errorf("persisted catalog has %d dir entries after a neutral prewarm, want 0 (leaked probe temp dir): %v", len(file.Dirs), file.Dirs)
	}
}
