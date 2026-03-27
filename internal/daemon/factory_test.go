package daemon_test

import (
	"context"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemon"
)

func TestOpenCodeBackendManagerCreateBackend(t *testing.T) {
	t.Parallel()
	// Smoke test: creating an OpenCodeBackendManager should not panic.
	mgr := daemon.NewOpenCodeBackendManager()
	defer mgr.Shutdown()
	_ = mgr
}

func TestClaudeBackendManagerCreateBackend(t *testing.T) {
	t.Parallel()
	mgr := daemon.NewClaudeBackendManager()
	defer mgr.Shutdown()

	backend, err := mgr.CreateBackend(agent.StartRequest{
		Backend:    agent.BackendClaudeCode,
		ProjectDir: "/tmp/test",
		SessionID:  "",
	})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}
	if backend == nil {
		t.Fatal("expected non-nil backend")
	}

	// Watch should be a no-op for Claude.
	if err := backend.Watch(context.Background()); err != nil {
		t.Fatalf("Watch: %v", err)
	}
}
