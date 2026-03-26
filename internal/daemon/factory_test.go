package daemon_test

import (
	"testing"

	"github.com/acksell/clank/internal/agent"
	"github.com/acksell/clank/internal/daemon"
)

func TestDefaultBackendFactoryUnknownType(t *testing.T) {
	factory := daemon.NewDefaultBackendFactory()
	defer factory.StopAll()

	_, err := factory.Create("unknown", agent.StartRequest{
		Backend:    "unknown",
		ProjectDir: "/tmp/test",
		Prompt:     "test",
	})
	if err == nil {
		t.Error("expected error for unknown backend type")
	}
}
