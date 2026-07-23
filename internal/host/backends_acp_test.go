package host_test

import (
	"context"
	"errors"
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
	var wg sync.WaitGroup
	var modes []agent.SessionMode
	var models []agent.ModelInfo
	wg.Add(2)
	go func() { defer wg.Done(); modes, _ = mgr.ListModes(ctx, workDir) }()
	go func() { defer wg.Done(); models, _ = mgr.ListModels(ctx, workDir) }()
	wg.Wait()

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
	if _, err := mgr.ListModes(ctx, workDir); err != nil {
		t.Fatalf("ListModes (warm): %v", err)
	}
	if _, err := mgr.ListModels(ctx, workDir); err != nil {
		t.Fatalf("ListModels (warm): %v", err)
	}
	if got := sessions.Load(); got != 1 {
		t.Errorf("warm reads opened %d sessions, want no new ones", got)
	}
}

// A transient probe failure (adapter not ready, timeout) must not
// permanently brick the picker: only a successful probe may mark a dir
// as probed, so the next caller gets to retry.
func TestACPBackendManager_ProbeFailureRetriesInsteadOfPermanentlyEmptyingCatalog(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr, err := host.NewCodexACPManager(acpDirs(t))
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
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

	// First probe fails: catalog stays empty, but the dir must NOT be
	// marked probed — otherwise it's stuck empty for the daemon's lifetime.
	models, err := mgr.ListModels(ctx, workDir)
	if err != nil {
		t.Fatalf("ListModels (failed probe): %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("ListModels after failed probe = %+v, want empty", models)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts after first call = %d, want 1", got)
	}

	// Second call retries the probe and succeeds.
	models, err = mgr.ListModels(ctx, workDir)
	if err != nil {
		t.Fatalf("ListModels (retried probe): %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5.2-codex" {
		t.Fatalf("ListModels after retried probe = %+v, want the agent's advertised catalog", models)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts after retried call = %d, want 2 (probe must retry after a failure)", got)
	}

	// Warm dir now: served from the catalog, no further probe attempts.
	if _, err := mgr.ListModels(ctx, workDir); err != nil {
		t.Fatalf("ListModels (warm): %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("warm read after success made %d attempts, want no new ones", got)
	}
}

// acpDirs gives a manager isolated on-disk state: a throwaway tools dir
// and a throwaway catalog, so a test never reads or writes the
// developer's real ~/.clank catalog.
func acpDirs(t *testing.T) host.ACPDirs {
	t.Helper()
	return host.ACPDirs{Tools: t.TempDir(), Catalog: t.TempDir()}
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
		mgr, err := host.NewCodexACPManager(host.ACPDirs{Tools: t.TempDir(), Catalog: catalogDir})
		if err != nil {
			t.Fatalf("NewCodexACPManager: %v", err)
		}
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
	if _, err := first.ListModels(ctx, workDir); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
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
