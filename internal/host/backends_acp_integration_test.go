package host_test

import (
	"context"
	"os"
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
