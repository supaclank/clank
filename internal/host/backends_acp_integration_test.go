package host_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

// Drives the full codex provisioning + spawn chain for real: acptools
// bun install of the pinned manifest, codex-acp under bun, initialize,
// session/new. Needs network + bun; gated. codex requires auth at
// session/new — with OPENAI_API_KEY set the test requires full success;
// without it the auth-required error IS the proof the chain works.
func TestIntegration_CodexACP_ProvisionSpawnOpen(t *testing.T) {
	if os.Getenv("CLANK_TEST_CODEX_ACP") == "" {
		t.Skip("set CLANK_TEST_CODEX_ACP=1 to run the real codex-acp provisioning chain")
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	t.Setenv("HOME", t.TempDir()) // isolate installGuidanceSkills

	mgr, err := host.NewCodexACPManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewCodexACPManager: %v", err)
	}
	if apiKey != "" {
		mgr.SetEnvResolver(func() map[string]string {
			return map[string]string{host.EnvCodexAPIKey: apiKey}
		})
	}
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

	openCtx, openCancel := context.WithTimeout(ctx, 4*time.Minute)
	defer openCancel()
	if err := b.Open(openCtx); err != nil {
		if apiKey == "" && strings.Contains(err.Error(), "Authentication required") {
			// The full chain ran: provisioning, spawn under bun,
			// initialize — codex just wants credentials for the thread.
			t.Logf("chain verified up to codex auth (no OPENAI_API_KEY set): %v", err)
			return
		}
		t.Fatalf("Open (provision + spawn + initialize + session/new): %v", err)
	}
	if b.SessionID() == "" {
		t.Fatal("no codex thread id after Open")
	}
	if cur, modes := b.(agent.ModeReporter).Modes(); len(modes) == 0 || cur == "" {
		t.Errorf("expected codex to advertise modes, got current=%q modes=%d", cur, len(modes))
	} else {
		t.Logf("codex modes: current=%s available=%v", cur, modes)
	}
}

// Resume must survive a full manager teardown (the daemon-restart
// shape): a fresh supervisor + adapter process rebuilds the session
// from the agent's own store via session/load. The bespoke↔ACP
// cross-impl variant of this gate ran before the bespoke deletion (M2);
// both directions passed against the shared session store.
func TestIntegration_OpenCodeACP_ResumeAcrossProcesses(t *testing.T) {
	if os.Getenv("CLANK_TEST_OPENCODE_ACP") == "" {
		t.Skip("set CLANK_TEST_OPENCODE_ACP=1 to run against the real opencode binary")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not on PATH")
	}
	// Fresh temp projects may have no usable model provider (e.g. only a
	// revoked copilot login), so a real project dir can be supplied.
	dir := os.Getenv("CLANK_TEST_OPENCODE_DIR")
	if dir == "" {
		dir = t.TempDir()
		if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
			t.Fatalf("git init: %v", err)
		}
	}

	waitIdle := func(b agent.SessionBackend, phase string) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			switch b.Status() {
			case agent.StatusIdle:
				return
			case agent.StatusError, agent.StatusDead:
				t.Fatalf("%s: status %s", phase, b.Status())
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: never reached idle", phase)
	}
	containsText := func(msgs []agent.MessageData, want string) bool {
		for _, m := range msgs {
			if strings.Contains(m.Content, want) {
				return true
			}
			for _, p := range m.Parts {
				if strings.Contains(p.Text, want) {
					return true
				}
			}
		}
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Phase 1: create + complete a turn.
	mgr1, err := host.NewOpenCodeACPManager()
	if err != nil {
		t.Fatalf("NewOpenCodeACPManager: %v", err)
	}
	mgr1.Supervisor().SetReconcileInterval(50 * time.Millisecond)
	if err := mgr1.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("mgr1 Init: %v", err)
	}
	b1, err := mgr1.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir})
	if err != nil {
		t.Fatalf("mgr1 CreateBackend: %v", err)
	}
	sendCtx, sendCancel := context.WithTimeout(ctx, 60*time.Second)
	if err := b1.OpenAndSend(sendCtx, agent.SendMessageOpts{Text: "Reply with exactly: XPROC_A", Model: &agent.ModelOverride{ProviderID: "opencode", ModelID: "big-pickle"}}); err != nil {
		sendCancel()
		t.Fatalf("OpenAndSend: %v", err)
	}
	sendCancel()
	waitIdle(b1, "first turn")
	ext := b1.SessionID()
	if ext == "" {
		t.Fatal("no external id after first turn")
	}
	_ = b1.Stop()
	mgr1.Shutdown()

	// Phase 2: fresh manager (fresh adapter process) resumes + continues.
	mgr2, err := host.NewOpenCodeACPManager()
	if err != nil {
		t.Fatalf("NewOpenCodeACPManager (2): %v", err)
	}
	mgr2.Supervisor().SetReconcileInterval(50 * time.Millisecond)
	if err := mgr2.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("mgr2 Init: %v", err)
	}
	defer mgr2.Shutdown()
	b2, err := mgr2.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir, ResumeExternalID: ext})
	if err != nil {
		t.Fatalf("mgr2 CreateBackend (resume): %v", err)
	}
	openCtx, openCancel := context.WithTimeout(ctx, 60*time.Second)
	if err := b2.Open(openCtx); err != nil {
		openCancel()
		t.Fatalf("Open (resume): %v", err)
	}
	openCancel()
	msgCtx, msgCancel := context.WithTimeout(ctx, 30*time.Second)
	msgs, err := b2.Messages(msgCtx)
	msgCancel()
	if err != nil {
		t.Fatalf("Messages after resume: %v", err)
	}
	if !containsText(msgs, "XPROC_A") {
		t.Fatalf("resume lost the first turn; got %d messages", len(msgs))
	}
	sendCtx2, sendCancel2 := context.WithTimeout(ctx, 60*time.Second)
	if err := b2.Send(sendCtx2, agent.SendMessageOpts{Text: "Reply with exactly: XPROC_B", Model: &agent.ModelOverride{ProviderID: "opencode", ModelID: "big-pickle"}}); err != nil {
		sendCancel2()
		t.Fatalf("Send after resume: %v", err)
	}
	sendCancel2()
	waitIdle(b2, "follow-up turn")
	_ = b2.Stop()
}

// The claude cutover gate (G-CLD-RESUME): the single biggest cross-impl
// unknown was whether the adapter's ACP sessionId equals the Claude CLI
// session uuid the bespoke backend persists as ExternalID. Both
// directions must hold for the M3 flip (and its rollback). Uses the
// machine's real claude login; costs two tiny turns.
func TestIntegration_ClaudeACP_CrossImplResume(t *testing.T) {
	if os.Getenv("CLANK_TEST_CLAUDE_ACP") == "" {
		t.Skip("set CLANK_TEST_CLAUDE_ACP=1 to run against the real claude login")
	}

	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	waitIdle := func(b agent.SessionBackend, phase string) {
		t.Helper()
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			switch b.Status() {
			case agent.StatusIdle:
				return
			case agent.StatusError, agent.StatusDead:
				t.Fatalf("%s: status %s", phase, b.Status())
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: never reached idle", phase)
	}
	containsText := func(msgs []agent.MessageData, want string) bool {
		for _, m := range msgs {
			if strings.Contains(m.Content, want) {
				return true
			}
			for _, p := range m.Parts {
				if strings.Contains(p.Text, want) {
					return true
				}
			}
		}
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Phase 1: bespoke claude creates a session and completes a turn.
	bespoke := host.NewClaudeBackendManager()
	defer bespoke.Shutdown()
	b1, err := bespoke.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir})
	if err != nil {
		t.Fatalf("bespoke CreateBackend: %v", err)
	}
	sendCtx, sendCancel := context.WithTimeout(ctx, 120*time.Second)
	if err := b1.OpenAndSend(sendCtx, agent.SendMessageOpts{Text: "Reply with exactly: XCLD_A"}); err != nil {
		sendCancel()
		t.Fatalf("bespoke OpenAndSend: %v", err)
	}
	sendCancel()
	waitIdle(b1, "bespoke turn")
	ext := b1.SessionID()
	if ext == "" {
		t.Fatal("bespoke backend has no external id")
	}
	t.Logf("bespoke claude session uuid: %s", ext)
	_ = b1.Stop()

	// Phase 2: the ACP adapter loads the bespoke session by that uuid.
	toolsDir := os.Getenv("CLANK_TEST_ACP_TOOLS_DIR")
	if toolsDir == "" {
		toolsDir = t.TempDir()
	}
	acpMgr, err := host.NewClaudeACPManager(toolsDir)
	if err != nil {
		t.Fatalf("NewClaudeACPManager: %v", err)
	}
	acpMgr.Supervisor().SetReconcileInterval(50 * time.Millisecond)
	if err := acpMgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("acp Init: %v", err)
	}
	defer acpMgr.Shutdown()
	b2, err := acpMgr.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir, ResumeExternalID: ext})
	if err != nil {
		t.Fatalf("acp CreateBackend (resume): %v", err)
	}
	openCtx, openCancel := context.WithTimeout(ctx, 4*time.Minute) // may include cold bun install
	if err := b2.Open(openCtx); err != nil {
		openCancel()
		t.Fatalf("acp Open (resume bespoke claude session): %v", err)
	}
	openCancel()
	msgCtx, msgCancel := context.WithTimeout(ctx, 30*time.Second)
	msgs, err := b2.Messages(msgCtx)
	msgCancel()
	if err != nil {
		t.Fatalf("Messages after acp resume: %v", err)
	}
	if !containsText(msgs, "XCLD_A") {
		t.Fatalf("acp replay lost the bespoke turn; got %d messages", len(msgs))
	}
	sendCtx2, sendCancel2 := context.WithTimeout(ctx, 120*time.Second)
	if err := b2.Send(sendCtx2, agent.SendMessageOpts{Text: "Reply with exactly: XCLD_B"}); err != nil {
		sendCancel2()
		t.Fatalf("acp Send after cross-impl resume: %v", err)
	}
	sendCancel2()
	waitIdle(b2, "acp follow-up turn")
	acpExt := b2.SessionID()
	t.Logf("acp claude session id: %s", acpExt)
	_ = b2.Stop()
	acpMgr.Shutdown()

	// Phase 3 (rollback direction): bespoke resumes the ACP-continued
	// session by the adapter's id and sees both turns.
	b3, err := bespoke.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir, ResumeExternalID: acpExt})
	if err != nil {
		t.Fatalf("bespoke CreateBackend (resume acp session): %v", err)
	}
	openCtx3, openCancel3 := context.WithTimeout(ctx, 120*time.Second)
	if err := b3.Open(openCtx3); err != nil {
		openCancel3()
		t.Fatalf("bespoke Open (rollback resume): %v", err)
	}
	openCancel3()
	msgCtx3, msgCancel3 := context.WithTimeout(ctx, 60*time.Second)
	msgs, err = b3.Messages(msgCtx3)
	msgCancel3()
	if err != nil {
		t.Fatalf("Messages after rollback resume: %v", err)
	}
	if !containsText(msgs, "XCLD_A") || !containsText(msgs, "XCLD_B") {
		t.Fatalf("rollback resume lost turns (A=%v B=%v)", containsText(msgs, "XCLD_A"), containsText(msgs, "XCLD_B"))
	}
	_ = b3.Stop()
}
