package agent

import (
	"testing"

	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// resumeTargetUUID maps a user-message revert target to the assistant message
// --resume-session-at should keep. White-box test (package agent) because the
// helper is unexported.
func TestResumeTargetUUID(t *testing.T) {
	t.Parallel()

	// hello → assistant → "add a line" (revert target) → assistant
	msgs := []claudecode.SessionMessage{
		{Type: "user", UUID: "u1"},
		{Type: "assistant", UUID: "a1"},
		{Type: "user", UUID: "u2"},
		{Type: "assistant", UUID: "a2"},
	}

	cases := []struct {
		name, target, want string
	}{
		{"keeps assistant before target", "u2", "a1"},
		{"first turn has no prior assistant", "u1", ""},
		{"absent target", "missing", ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := resumeTargetUUID(msgs, c.target); got != c.want {
				t.Errorf("resumeTargetUUID(_, %q) = %q, want %q", c.target, got, c.want)
			}
		})
	}

	t.Run("skips meta records before the target", func(t *testing.T) {
		t.Parallel()
		withMeta := []claudecode.SessionMessage{
			{Type: "assistant", UUID: "a1"},
			{Type: "user", UUID: "meta", IsMeta: true},
			{Type: "user", UUID: "u2"},
		}
		if got := resumeTargetUUID(withMeta, "u2"); got != "a1" {
			t.Errorf("got %q, want a1", got)
		}
	})
}
