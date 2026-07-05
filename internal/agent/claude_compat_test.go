package agent

import "testing"

// TestParseClaudeVersionOutput pins the suffixed `claude --version`
// output shape ("2.1.201 (Claude Code)") — exact-matching the raw
// line against PinnedClaudeVersion would never succeed, which is the
// bug this parser exists to prevent.
func TestParseClaudeVersionOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"2.1.201 (Claude Code)", "2.1.201"},
		{"2.1.201 (Claude Code)\n", "2.1.201"},
		{"2.1.201", "2.1.201"},
		{"  2.1.201  ", "2.1.201"},
		{"", ""},
		{"   \n", ""},
	}
	for _, c := range cases {
		if got := ParseClaudeVersionOutput(c.in); got != c.want {
			t.Errorf("ParseClaudeVersionOutput(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
