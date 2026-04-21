package host_test

import (
	"context"
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/host"
)

func TestOpenCodeBackendManagerCreateBackend(t *testing.T) {
	t.Parallel()
	// Smoke test: creating an OpenCodeBackendManager should not panic.
	mgr := host.NewOpenCodeBackendManager()
	defer mgr.Shutdown()
	_ = mgr
}

func TestClaudeBackendManagerCreateBackend(t *testing.T) {
	t.Parallel()
	mgr := host.NewClaudeBackendManager()
	defer mgr.Shutdown()

	backend, err := mgr.CreateBackend(context.Background(), agent.BackendInvocation{
		WorkDir: "/tmp/test",
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
