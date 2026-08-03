package acp_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	acpx "github.com/supaclank/clank/internal/agent/acp"
	sdk "github.com/coder/acp-go-sdk"
)

// Exercises the production spawn path (execSpawn → stdio → initialize)
// against the machine's real opencode binary. Gated: set
// CLANK_TEST_OPENCODE_ACP=1 to run.
func TestIntegration_OpenCodeACP_SpawnInitializeNewSession(t *testing.T) {
	if os.Getenv("CLANK_TEST_OPENCODE_ACP") == "" {
		t.Skip("set CLANK_TEST_OPENCODE_ACP=1 to run against the real opencode binary")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not on PATH")
	}

	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	sup, err := acpx.NewAdapterSupervisor(acpx.OpenCodeProfile("opencode"), testLogf(t))
	if err != nil {
		t.Fatalf("NewAdapterSupervisor: %v", err)
	}
	runSupervisor(t, sup)

	ctx, callCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer callCancel()
	conn, err := sup.GetConn(ctx, dir)
	if err != nil {
		t.Fatalf("GetConn: %v", err)
	}

	init := conn.Init()
	if init.ProtocolVersion != sdk.ProtocolVersionNumber {
		t.Errorf("protocolVersion = %d, want %d", init.ProtocolVersion, sdk.ProtocolVersionNumber)
	}
	if !init.AgentCapabilities.LoadSession {
		t.Error("expected loadSession capability from opencode acp")
	}
	if init.AgentCapabilities.SessionCapabilities.List == nil {
		t.Error("expected session/list capability from opencode acp")
	}

	ns, err := conn.Conn().NewSession(ctx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if ns.SessionId == "" {
		t.Error("session/new returned empty session id")
	}
}
