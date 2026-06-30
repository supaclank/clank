package agent_test

import (
	"testing"

	"github.com/acksell/clank/internal/agent"
)

// A fresh Claude session must launch with --append-system-prompt carrying the
// guidance, appended to (not replacing) Claude's base system prompt.
func TestClaudeCodeBackend_SystemPrompt_FreshSessionAppends(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackend(t.TempDir())
	b.SystemPrompt = "EXPO GUIDANCE"
	defer b.Stop()

	resolved := captureOpenOptions(t, b, transport)
	if resolved.AppendSystemPrompt == nil {
		t.Fatal("fresh session: AppendSystemPrompt is nil, want guidance appended")
	}
	if *resolved.AppendSystemPrompt != "EXPO GUIDANCE" {
		t.Errorf("AppendSystemPrompt = %q, want %q", *resolved.AppendSystemPrompt, "EXPO GUIDANCE")
	}
}

// A resumed session must not re-append guidance — even if the field is set —
// because the guidance already shaped the conversation being continued. Guards
// the Open()-level resumeID check independently of the host's CreateBackend.
func TestClaudeCodeBackend_SystemPrompt_ResumeDoesNotAppend(t *testing.T) {
	t.Parallel()
	transport := newMockTransport(nil)
	b := agent.NewClaudeCodeBackendForSession(t.TempDir(), "resume-session-id")
	b.SystemPrompt = "EXPO GUIDANCE"
	defer b.Stop()

	resolved := captureOpenOptions(t, b, transport)
	if resolved.AppendSystemPrompt != nil {
		t.Errorf("resumed session: AppendSystemPrompt = %q, want nil", *resolved.AppendSystemPrompt)
	}
}
