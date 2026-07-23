package acp_test

import (
	"context"
	"testing"
	"time"

	acpx "github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acp/acptest"
	sdk "github.com/coder/acp-go-sdk"
)

type captureHandler struct {
	updates chan sdk.SessionNotification
	allowID sdk.PermissionOptionId
}

func (h *captureHandler) HandleSessionUpdate(_ context.Context, n sdk.SessionNotification) {
	h.updates <- n
}

func (h *captureHandler) HandleRequestPermission(_ context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected(h.allowID)}, nil
}

func TestConn_RoutesUpdatesAndPermissions(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentImpl := &acptest.ScriptedAgent{}
	proc, err := acptest.Proc(ctx, testProfile(acpx.ScopeHost, nil), agentImpl, t.Logf)
	if err != nil {
		t.Fatalf("Proc: %v", err)
	}
	defer proc.Stop()

	h := &captureHandler{updates: make(chan sdk.SessionNotification, 8), allowID: "opt-allow"}
	proc.Conn.Register("s1", h)

	// Routed update reaches the handler.
	if err := agentImpl.Conn().SessionUpdate(ctx, sdk.SessionNotification{
		SessionId: "s1",
		Update:    sdk.UpdateAgentMessageText("hi"),
	}); err != nil {
		t.Fatalf("agent SessionUpdate: %v", err)
	}
	select {
	case n := <-h.updates:
		if n.Update.AgentMessageChunk == nil {
			t.Fatalf("unexpected update shape: %+v", n.Update)
		}
	case <-ctx.Done():
		t.Fatal("routed update never arrived")
	}

	// Unrouted update is dropped without wedging the stream.
	if err := agentImpl.Conn().SessionUpdate(ctx, sdk.SessionNotification{
		SessionId: "ghost",
		Update:    sdk.UpdateAgentMessageText("void"),
	}); err != nil {
		t.Fatalf("agent SessionUpdate (unrouted): %v", err)
	}

	// Routed permission request gets the handler's decision.
	resp, err := agentImpl.Conn().RequestPermission(ctx, sdk.RequestPermissionRequest{
		SessionId: "s1",
		Options:   []sdk.PermissionOption{{OptionId: "opt-allow", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce}},
	})
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "opt-allow" {
		t.Fatalf("outcome = %+v, want selected opt-allow", resp.Outcome)
	}

	// Unrouted permission resolves cancelled instead of erroring, so a
	// prompt racing session teardown can't wedge the agent.
	resp, err = agentImpl.Conn().RequestPermission(ctx, sdk.RequestPermissionRequest{
		SessionId: "ghost",
		Options:   []sdk.PermissionOption{{OptionId: "x", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce}},
	})
	if err != nil {
		t.Fatalf("RequestPermission (unrouted): %v", err)
	}
	if resp.Outcome.Cancelled == nil {
		t.Fatalf("unrouted outcome = %+v, want cancelled", resp.Outcome)
	}

	// After Deregister the session routes nowhere again.
	proc.Conn.Deregister("s1")
	if err := agentImpl.Conn().SessionUpdate(ctx, sdk.SessionNotification{
		SessionId: "s1",
		Update:    sdk.UpdateAgentMessageText("late"),
	}); err != nil {
		t.Fatalf("agent SessionUpdate (deregistered): %v", err)
	}
	select {
	case n := <-h.updates:
		t.Fatalf("update delivered after Deregister: %+v", n)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestConn_ProtocolVersionMismatchFailsConstruction(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentImpl := &acptest.ScriptedAgent{
		InitializeFn: func(context.Context, sdk.InitializeRequest) (sdk.InitializeResponse, error) {
			return sdk.InitializeResponse{ProtocolVersion: 99}, nil
		},
	}
	if _, err := acptest.Proc(ctx, testProfile(acpx.ScopeHost, nil), agentImpl, t.Logf); err == nil {
		t.Fatal("expected protocol-version mismatch error")
	}
}
