package agent_test

import (
	"slices"
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
		{"codex", agent.BackendCodex, false},
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

func TestParseBackendSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    []agent.BackendType
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"none", "none", nil, false},
		{"all", "all", agent.AllBackends, false},
		{"single", "codex", []agent.BackendType{agent.BackendCodex}, false},
		{"multi_with_alias", "claude, opencode", []agent.BackendType{agent.BackendClaudeCode, agent.BackendOpenCode}, false},
		{"dupes_collapse", "codex,codex", []agent.BackendType{agent.BackendCodex}, false},
		{"unknown_token", "codex,bogus", nil, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := agent.ParseBackendSet(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err: got %v, wantErr=%v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// StartRequest.Validate accepts exactly the backends in AllBackends: codex
// is declared but stays invalid until its manager registers (it joins
// AllBackends in the ACP-migration slice that ships the manager).
func TestStartRequestValidate_BackendsFollowRegistry(t *testing.T) {
	t.Parallel()

	for _, bt := range agent.AllBackends {
		req := agent.StartRequest{Backend: bt, GitRef: agent.GitRef{LocalPath: "/tmp/repo"}, Prompt: "hi"}
		if err := req.Validate(); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", bt, err)
		}
	}
	if slices.Contains(agent.AllBackends, agent.BackendCodex) {
		t.Skip("codex has joined AllBackends; the rejection half of this test is obsolete")
	}
	req := agent.StartRequest{Backend: agent.BackendCodex, GitRef: agent.GitRef{LocalPath: "/tmp/repo"}, Prompt: "hi"}
	if err := req.Validate(); err == nil {
		t.Error("Validate(codex) = nil, want error while codex is not in AllBackends")
	}
}
