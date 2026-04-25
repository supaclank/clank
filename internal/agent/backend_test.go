package agent_test

import (
	"testing"

	"github.com/acksell/clank/internal/agent"
)

func TestParseBackend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    agent.BackendType
		wantErr bool
	}{
		{"opencode", agent.BackendOpenCode, false},
		{"claude-code", agent.BackendClaudeCode, false},
		{"claude", agent.BackendClaudeCode, false}, // alias
		{"", "", true},
		{"unknown", "", true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := agent.ParseBackend(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err: got %v, wantErr=%v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveBackendPreference(t *testing.T) {
	t.Parallel()

	t.Run("empty falls back to default", func(t *testing.T) {
		t.Parallel()
		got, err := agent.ResolveBackendPreference("")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != agent.DefaultBackend {
			t.Errorf("got %q, want %q", got, agent.DefaultBackend)
		}
	})

	t.Run("valid value parsed", func(t *testing.T) {
		t.Parallel()
		got, err := agent.ResolveBackendPreference("claude-code")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != agent.BackendClaudeCode {
			t.Errorf("got %q, want claude-code", got)
		}
	})

	t.Run("invalid value falls back to default with error", func(t *testing.T) {
		t.Parallel()
		got, err := agent.ResolveBackendPreference("nope")
		if err == nil {
			t.Fatal("expected error for invalid value")
		}
		if got != agent.DefaultBackend {
			t.Errorf("got %q, want default %q", got, agent.DefaultBackend)
		}
	})
}

// TestDefaultBackend_StableContract pins down the default so a behavioural
// change (switching the project default) becomes an explicit code review
// signal rather than a silent diff in another file.
func TestDefaultBackend_StableContract(t *testing.T) {
	t.Parallel()
	if agent.DefaultBackend != agent.BackendOpenCode {
		t.Errorf("DefaultBackend changed: got %q, want opencode — update docs/UI before adjusting this test", agent.DefaultBackend)
	}
}
