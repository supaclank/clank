package acp_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
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

// Exercises the production spawn path against the machine's real hermes
// install (`hermes acp`). Gated: set CLANK_TEST_HERMES_ACP=1 to run.
// session/new needs no credentials (auth bites at prompt time), so this
// asserts the full advertised surface incl. the preset mode vocabulary.
func TestIntegration_HermesACP_SpawnInitializeNewSession(t *testing.T) {
	if os.Getenv("CLANK_TEST_HERMES_ACP") == "" {
		t.Skip("set CLANK_TEST_HERMES_ACP=1 to run against the real hermes binary")
	}
	if _, err := exec.LookPath("hermes"); err != nil {
		t.Skip("hermes not on PATH")
	}

	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	sup, err := acpx.NewAdapterSupervisor(acpx.HermesProfile("hermes"), testLogf(t))
	if err != nil {
		t.Fatalf("NewAdapterSupervisor: %v", err)
	}
	runSupervisor(t, sup)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, err := sup.GetConn(ctx, dir)
	if err != nil {
		t.Fatalf("GetConn: %v", err)
	}

	init := conn.Init()
	if init.ProtocolVersion != sdk.ProtocolVersionNumber {
		t.Errorf("protocolVersion = %d, want %d", init.ProtocolVersion, sdk.ProtocolVersionNumber)
	}
	if !init.AgentCapabilities.LoadSession {
		t.Error("expected loadSession capability from hermes acp")
	}
	if init.AgentCapabilities.SessionCapabilities.List == nil {
		t.Error("expected session/list capability from hermes acp")
	}

	ns, err := conn.Conn().NewSession(ctx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if ns.SessionId == "" {
		t.Error("session/new returned empty session id")
	}
	if ns.Modes == nil {
		t.Fatal("expected advertised session modes from hermes acp")
	}
	// The built-in presets reference these ids; a vocabulary change on a
	// floor bump must fail here, not in production.
	want := map[string]bool{"default": false, "accept_edits": false, "dont_ask": false}
	for _, m := range ns.Modes.AvailableModes {
		if _, ok := want[string(m.Id)]; ok {
			want[string(m.Id)] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("hermes did not advertise mode %q (preset vocabulary drift)", id)
		}
	}
}

// Exercises the production spawn path against the real pinned pi-acp +
// pi pair (provisioned via acptools, adapter under bun, pi spawned via
// the materialized bun shim). Gated: set CLANK_TEST_PI_ACP=1 to run.
// With a configured pi (~/.pi/agent/models.json or a login) session/new
// must advertise the model/thought_level options and thinking-level
// modes; without one it must fail with pi's auth error while the
// adapter stays alive.
func TestIntegration_PiACP_SpawnInitializeNewSession(t *testing.T) {
	if os.Getenv("CLANK_TEST_PI_ACP") == "" {
		t.Skip("set CLANK_TEST_PI_ACP=1 to run against the real pinned pi-acp")
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	toolsDir := t.TempDir()
	tools, err := acptools.Ensure(ctx, toolsDir)
	if err != nil {
		t.Fatalf("acptools.Ensure: %v", err)
	}

	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	profile := acpx.PiProfile(tools.BunBin, tools.PiACPEntry, tools.PiWrapper)
	sup, err := acpx.NewAdapterSupervisor(profile, testLogf(t))
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
		t.Error("expected loadSession capability from pi-acp")
	}
	if init.AgentCapabilities.SessionCapabilities.List == nil {
		t.Error("expected session/list capability from pi-acp")
	}

	ns, err := conn.Conn().NewSession(ctx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		// Unconfigured pi: an auth-shaped failure, not a dead adapter.
		select {
		case <-conn.Closed():
			t.Fatal("adapter conn died after unconfigured session/new")
		default:
		}
		t.Logf("session/new requires a configured pi: %v", err)
		return
	}
	if ns.SessionId == "" {
		t.Error("session/new returned empty session id")
	}
	if ns.Modes == nil || len(ns.Modes.AvailableModes) == 0 {
		t.Fatal("expected thinking-level modes from pi-acp")
	}
	// The built-in presets reference mode id "off" (thinking level).
	found := false
	for _, m := range ns.Modes.AvailableModes {
		if string(m.Id) == "off" {
			found = true
		}
	}
	if !found {
		t.Error("pi did not advertise mode \"off\" (preset vocabulary drift)")
	}
}

// Drives a full turn through the production Backend (Open → Send →
// Events) against the pinned pi pair. Gated on CLANK_TEST_PI_ACP_TURN=1
// because it needs a configured pi (~/.pi/agent/models.json — a local
// OpenAI-compatible server is enough) and burns a real completion.
func TestIntegration_PiACP_FullTurn(t *testing.T) {
	if os.Getenv("CLANK_TEST_PI_ACP_TURN") == "" {
		t.Skip("set CLANK_TEST_PI_ACP_TURN=1 (needs a configured pi) to run")
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

	profile := acpx.PiProfile(tools.BunBin, tools.PiACPEntry, tools.PiWrapper)
	sup, err := acpx.NewAdapterSupervisor(profile, testLogf(t))
	if err != nil {
		t.Fatalf("NewAdapterSupervisor: %v", err)
	}
	runSupervisor(t, sup)

	resolver := func(ctx context.Context) (*acpx.AdapterConn, error) { return sup.GetConn(ctx, dir) }
	b := acpx.NewBackend(profile, dir, "", "", resolver, testLogf(t))
	defer func() { _ = b.Stop() }()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Send(ctx, agent.SendMessageOpts{Text: "Reply with exactly: SPIKE_OK"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// pi streams pre-turn agent chunks (its skills preamble) right after
	// session/new, so only the busy→idle cycle bounds the actual turn;
	// accumulation resets when the prompt dispatches.
	var assistant string
	sawBusy := false
	for {
		select {
		case e := <-b.Events():
			switch e.Type {
			case agent.EventPartUpdate:
				if p := e.Data.(agent.PartUpdateData); p.Part.Type == agent.PartText && p.IsDelta {
					assistant += p.Part.Text
				}
			case agent.EventStatusChange:
				switch e.Data.(agent.StatusChangeData).NewStatus {
				case agent.StatusBusy:
					sawBusy = true
					assistant = ""
				case agent.StatusIdle:
					if !sawBusy {
						continue
					}
					if !strings.Contains(assistant, "SPIKE_OK") {
						t.Fatalf("assistant said %q, want SPIKE_OK", assistant)
					}
					return
				}
			case agent.EventError:
				t.Fatalf("backend error: %+v", e.Data)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for the turn; assistant so far: %q", assistant)
		}
	}
}

// Drives a full turn through the production Backend (Open → Send →
// Events) against the machine's real hermes install. Gated on
// CLANK_TEST_HERMES_ACP_TURN=1 because it needs a hermes with a working
// model provider configured (any provider — a local OpenAI-compatible
// server is enough) and burns a real completion.
func TestIntegration_HermesACP_FullTurn(t *testing.T) {
	if os.Getenv("CLANK_TEST_HERMES_ACP_TURN") == "" {
		t.Skip("set CLANK_TEST_HERMES_ACP_TURN=1 (needs a configured hermes install) to run")
	}
	if _, err := exec.LookPath("hermes"); err != nil {
		t.Skip("hermes not on PATH")
	}

	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	sup, err := acpx.NewAdapterSupervisor(acpx.HermesProfile("hermes"), testLogf(t))
	if err != nil {
		t.Fatalf("NewAdapterSupervisor: %v", err)
	}
	runSupervisor(t, sup)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	resolver := func(ctx context.Context) (*acpx.AdapterConn, error) { return sup.GetConn(ctx, dir) }
	b := acpx.NewBackend(acpx.HermesProfile("hermes"), dir, "", "", resolver, testLogf(t))
	defer func() { _ = b.Stop() }()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Send(ctx, agent.SendMessageOpts{Text: "Reply with exactly: SPIKE_OK"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The busy→idle cycle bounds the turn; text accumulates from part
	// deltas (the assistant message event is a content-less shell).
	var assistant string
	sawBusy := false
	for {
		select {
		case e := <-b.Events():
			switch e.Type {
			case agent.EventPartUpdate:
				if p := e.Data.(agent.PartUpdateData); p.Part.Type == agent.PartText && p.IsDelta {
					assistant += p.Part.Text
				}
			case agent.EventStatusChange:
				switch e.Data.(agent.StatusChangeData).NewStatus {
				case agent.StatusBusy:
					sawBusy = true
					assistant = ""
				case agent.StatusIdle:
					if !sawBusy {
						continue
					}
					if !strings.Contains(assistant, "SPIKE_OK") {
						t.Fatalf("assistant said %q, want SPIKE_OK", assistant)
					}
					return
				}
			case agent.EventError:
				t.Fatalf("backend error: %+v", e.Data)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for the turn; assistant so far: %q", assistant)
		}
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
