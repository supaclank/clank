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

	mgr, err := host.NewCodexACPManager("unused-bun", "unused-entry", nil)
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
