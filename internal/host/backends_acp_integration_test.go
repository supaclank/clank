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

// The opencode cutover gate (G-OC-RESUME): sessions must resume across
// implementations in BOTH directions, because rollback is an env flip.
// Bespoke (`opencode serve` + SDK) and ACP (`opencode acp`) share the
// same binary and session store, so the external id is the bridge.
// Sequential phases — production never runs both paths on one dir at
// once (toggle = restart), and neither does this test.
func TestIntegration_OpenCodeACP_CrossImplResume(t *testing.T) {
	if os.Getenv("CLANK_TEST_OPENCODE_ACP") == "" {
		t.Skip("set CLANK_TEST_OPENCODE_ACP=1 to run against the real opencode binary")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not on PATH")
	}
	// Real HOME on purpose: the user's opencode store IS the fixture.
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
	transcript := func(b agent.SessionBackend, phase string) []agent.MessageData {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		msgs, err := b.Messages(ctx)
		if err != nil {
			t.Fatalf("%s: Messages: %v", phase, err)
		}
		return msgs
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

	// Phase 1: bespoke creates a session and completes a turn.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bespoke := host.NewOpenCodeBackendManager()
	if err := bespoke.Init(ctx, func() ([]string, error) { return []string{dir}, nil }); err != nil {
		t.Fatalf("bespoke Init: %v", err)
	}
	b1, err := bespoke.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir})
	if err != nil {
		t.Fatalf("bespoke CreateBackend: %v", err)
	}
	go func() {
		for e := range b1.Events() {
			t.Logf("bespoke event: %s %+v", e.Type, e.Data)
		}
	}()
	sendCtx, sendCancel := context.WithTimeout(ctx, 60*time.Second)
	if err := b1.OpenAndSend(sendCtx, agent.SendMessageOpts{Text: "Reply with exactly: XIMPL_A", Model: &agent.ModelOverride{ProviderID: "opencode", ModelID: "big-pickle"}}); err != nil {
		sendCancel()
		t.Fatalf("bespoke OpenAndSend: %v", err)
	}
	sendCancel()
	waitIdle(b1, "bespoke turn")
	ext := b1.SessionID()
	if ext == "" {
		t.Fatal("bespoke backend has no external id")
	}
	_ = b1.Stop()
	bespoke.Shutdown() // sequential phases: no double server on one dir

	// Phase 2: ACP resumes the bespoke-created session and continues it.
	acpMgr, err := host.NewOpenCodeACPManager()
	if err != nil {
		t.Fatalf("NewOpenCodeACPManager: %v", err)
	}
	acpMgr.Supervisor().SetReconcileInterval(50 * time.Millisecond)
	if err := acpMgr.Init(ctx, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("acp Init: %v", err)
	}
	b2, err := acpMgr.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir, ResumeExternalID: ext})
	if err != nil {
		t.Fatalf("acp CreateBackend (resume): %v", err)
	}
	openCtx, openCancel := context.WithTimeout(ctx, 60*time.Second)
	if err := b2.Open(openCtx); err != nil {
		openCancel()
		t.Fatalf("acp Open (resume bespoke session): %v", err)
	}
	openCancel()
	msgs := transcript(b2, "acp resume")
	if !containsText(msgs, "XIMPL_A") {
		t.Fatalf("acp replay lost the bespoke turn; got %d messages", len(msgs))
	}
	sendCtx2, sendCancel2 := context.WithTimeout(ctx, 60*time.Second)
	if err := b2.Send(sendCtx2, agent.SendMessageOpts{Text: "Reply with exactly: XIMPL_B", Model: &agent.ModelOverride{ProviderID: "opencode", ModelID: "big-pickle"}}); err != nil {
		sendCancel2()
		t.Fatalf("acp Send after cross-impl resume: %v", err)
	}
	sendCancel2()
	waitIdle(b2, "acp follow-up turn")
	if msgs = transcript(b2, "acp after follow-up"); !containsText(msgs, "XIMPL_B") {
		t.Fatal("acp follow-up reply missing")
	}
	_ = b2.Stop()
	acpMgr.Shutdown()

	// Phase 3 (rollback direction): bespoke resumes the ACP-continued
	// session and sees both turns.
	bespoke2 := host.NewOpenCodeBackendManager()
	if err := bespoke2.Init(ctx, func() ([]string, error) { return []string{dir}, nil }); err != nil {
		t.Fatalf("bespoke2 Init: %v", err)
	}
	defer bespoke2.Shutdown()
	b3, err := bespoke2.CreateBackend(ctx, agent.BackendInvocation{WorkDir: dir, ResumeExternalID: ext})
	if err != nil {
		t.Fatalf("bespoke CreateBackend (resume acp session): %v", err)
	}
	openCtx3, openCancel3 := context.WithTimeout(ctx, 60*time.Second)
	if err := b3.Open(openCtx3); err != nil {
		openCancel3()
		t.Fatalf("bespoke Open (rollback resume): %v", err)
	}
	openCancel3()
	msgs = transcript(b3, "bespoke rollback resume")
	if !containsText(msgs, "XIMPL_A") || !containsText(msgs, "XIMPL_B") {
		t.Fatalf("rollback resume lost turns (A=%v B=%v)", containsText(msgs, "XIMPL_A"), containsText(msgs, "XIMPL_B"))
	}
	_ = b3.Stop()
}
