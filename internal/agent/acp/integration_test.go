package acp_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	acpx "github.com/acksell/clank/internal/agent/acp"
	"github.com/acksell/clank/internal/agent/acptools"
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

// Exercises the production spawn path against the real pinned gemini-cli
// (provisioned via acptools into a temp dir, run under bun). Gated: set
// CLANK_TEST_GEMINI_ACP=1 to run. Without GEMINI_API_KEY (or a cached
// Google login) session/new must fail with gemini's auth error while the
// adapter stays alive; with auth it must return a session advertising
// the mode ids the built-in presets reference.
func TestIntegration_GeminiACP_SpawnInitializeNewSession(t *testing.T) {
	if os.Getenv("CLANK_TEST_GEMINI_ACP") == "" {
		t.Skip("set CLANK_TEST_GEMINI_ACP=1 to run against the real pinned gemini-cli")
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tools, err := acptools.Ensure(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("acptools.Ensure: %v", err)
	}

	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	sup, err := acpx.NewAdapterSupervisor(acpx.GeminiProfile(tools.BunBin, tools.GeminiACPEntry), testLogf(t))
	if err != nil {
		t.Fatalf("NewAdapterSupervisor: %v", err)
	}
	runSupervisor(t, sup)

	conn, err := sup.GetConn(ctx, dir)
	if err != nil {
		t.Fatalf("GetConn: %v", err)
	}

	init := conn.Init()
	if init.ProtocolVersion != sdk.ProtocolVersionNumber {
		t.Errorf("protocolVersion = %d, want %d", init.ProtocolVersion, sdk.ProtocolVersionNumber)
	}
	if !init.AgentCapabilities.LoadSession {
		t.Error("expected loadSession capability from gemini --acp")
	}
	// gemini-cli advertises no session/list — the discovery paths rely on
	// this staying gated rather than erroring (see ACPBackendManager).
	if init.AgentCapabilities.SessionCapabilities.List != nil {
		t.Log("gemini now advertises session/list — the discovery gate can be revisited")
	}
	if len(init.AuthMethods) == 0 {
		t.Error("expected advertised authMethods from gemini --acp")
	}

	ns, err := conn.Conn().NewSession(ctx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		// The unauthenticated path: a clear auth error, not a dead adapter.
		if !strings.Contains(err.Error(), "API key") && !strings.Contains(err.Error(), "auth") {
			t.Fatalf("session/new failed with a non-auth error: %v", err)
		}
		select {
		case <-conn.Closed():
			t.Fatal("adapter conn died after unauthenticated session/new")
		default:
		}
		t.Logf("session/new correctly requires auth: %v", err)
		return
	}

	if ns.SessionId == "" {
		t.Error("session/new returned empty session id")
	}
	if ns.Modes == nil {
		t.Fatal("expected advertised session modes from gemini --acp")
	}
	// The built-in presets reference these ids (yolo/default/plan); a
	// vocabulary change on a pin bump must fail here, not in production.
	want := map[string]bool{"default": false, "autoEdit": false, "yolo": false, "plan": false}
	for _, m := range ns.Modes.AvailableModes {
		if _, ok := want[string(m.Id)]; ok {
			want[string(m.Id)] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("gemini did not advertise mode %q (preset vocabulary drift)", id)
		}
	}
}
