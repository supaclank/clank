package host_test

import (
	"context"
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

	mgr, err := host.NewCodexACPManager(t.TempDir())
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
	mgr, err := host.NewCodexACPManager(t.TempDir())
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

	mgr, err := host.NewCodexACPManager(t.TempDir())
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

	workDir := t.TempDir()
	if models, err := mgr.ListModels(ctx, workDir); err != nil || len(models) != 0 {
		t.Fatalf("ListModels before any session = %v/%v, want empty (no session has reported yet)", models, err)
	}

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

// ListModels' and ListModes' own doc comments (and this feature's PR
// description) promise that host-scoped profiles — codex, claude; their
// vocabulary doesn't vary by project dir — fall back to any known catalog
// for a dir that hasn't opened a session yet. The implementation was
// strictly per-dir with no such fallback; this pins the documented
// behavior.
func TestACPBackendManager_ListModelsAndModesFallBackForHostScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // installGuidanceSkills writes under $HOME

	mgr, err := host.NewCodexACPManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	category := sdk.SessionConfigOptionCategory("model")
	scripted := func(string) *acptest.ScriptedAgent {
		a := &acptest.ScriptedAgent{}
		a.NewSessionFn = func(ctx context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			return sdk.NewSessionResponse{
				SessionId: "s-1",
				Modes: &sdk.SessionModeState{
					CurrentModeId:  "agent",
					AvailableModes: []sdk.SessionMode{{Id: "agent", Name: "Agent"}},
				},
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

	seenDir := t.TempDir()
	b, err := mgr.CreateBackend(ctx, agent.BackendInvocation{WorkDir: seenDir})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}
	defer func() { _ = b.Stop() }()
	openCtx, openCancel := context.WithTimeout(ctx, 10*time.Second)
	defer openCancel()
	if err := b.Open(openCtx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	unseenDir := t.TempDir()
	models, err := mgr.ListModels(ctx, unseenDir)
	if err != nil || len(models) != 1 || models[0].ID != "gpt-5.2-codex" {
		t.Fatalf("ListModels(unseen dir) = %+v/%v, want fallback to the one known catalog (host-scoped profile)", models, err)
	}
	modes, err := mgr.ListModes(ctx, unseenDir)
	if err != nil || len(modes) != 1 || modes[0].ID != "agent" {
		t.Fatalf("ListModes(unseen dir) = %+v/%v, want fallback to the one known catalog (host-scoped profile)", modes, err)
	}
}
